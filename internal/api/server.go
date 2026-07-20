// Package api is Daffa's HTTP surface: the JSON API the SPA talks to, plus the
// static SPA itself.
package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/backups"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/config"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/monitor"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
	"github.com/Mnshahawy/daffa/internal/web"
)

type Server struct {
	cfg      *config.Config
	store    *store.Store
	sessions *auth.Manager
	pool     *dockerx.Pool
	// oidc holds the relying party for each configured identity provider. There may be
	// several, and they are rows in the database rather than environment variables.
	oidc    *auth.Registry
	limiter *auth.Limiter
	// caps memoises each user's effective capability mask. Every write to roles or
	// memberships invalidates it.
	caps *caps.Cache
	// notify turns events into outbox rows. A worker drains them; nothing here sends.
	notify *notify.Notifier
	// sealer encrypts secrets at rest: registry passwords, git tokens, stack env values.
	sealer *config.Sealer

	// oidcStates holds in-flight authorization requests (state → PKCE verifier + provider).
	oidcStates *stateStore
	// agents holds the tunnels of the agents currently connected.
	agents *registry
	// ssh manages the outbound SSH connections to remote clusters — the server-side mirror of
	// the agents registry, since for SSH it is Daffa that dials out (docs/clusters.md §4).
	ssh *sshManager
	// sched runs backup jobs on their cron expressions.
	sched *scheduler
	// collector samples CPU and memory; retention expires the samples.
	collector *monitor.Collector
	retention *monitor.Retention
	// certAlarms keeps the certificate worker's escalation memory, so an hourly sweep
	// warns once per stage instead of once per hour.
	certAlarms *certAlarms

	// Background-worker lifecycle. Start derives workerCtx from the process context and runs
	// every long-lived loop under wg; Stop cancels workerCtx and waits for them, so a shutdown
	// drains its workers instead of abandoning them mid-loop. See Start/Stop.
	wg         sync.WaitGroup
	stopWorker context.CancelFunc
	// workerCtx is the background-worker context, set by Start. Handlers that spawn long-lived
	// work (an SSH cluster's connect loop) parent it here so Stop winds it down too.
	workerCtx context.Context
}

func NewServer(cfg *config.Config, st *store.Store, pool *dockerx.Pool, sealer *config.Sealer) *Server {
	capCache := caps.NewCache(st)
	s := &Server{
		cfg:      cfg,
		store:    st,
		pool:     pool,
		oidc:     auth.NewRegistry(st, sealer),
		caps:     capCache,
		notify:   notify.New(st, sealer, slog.Default()),
		sealer:   sealer,
		sessions: auth.NewManager(st, capCache, cfg.SessionTTL, cfg.SecureCookie),
		// Ten failures per user+IP in fifteen minutes. Generous for a human who
		// forgot which password this is, useless for a guesser.
		limiter:    auth.NewLimiter(10, 15*time.Minute),
		oidcStates: newStateStore(),
		agents:     newRegistry(),
		ssh:        newSSHManager(),
		sched:      newScheduler(),
		collector:  monitor.NewCollector(st, pool, slog.Default()),
		retention:  monitor.NewRetention(st, slog.Default()),
		certAlarms: &certAlarms{},
	}

	// The evaluator runs on the back of each collection round rather than on a ticker of its
	// own, so it always sees the sample that was just written. Two tickers would race, and the
	// loser would keep evaluating a window one interval stale.
	eval := monitor.NewEvaluator(st, s.notify, slog.Default())
	s.collector.OnRound(eval.Run)

	return s
}

// Start brings up the background work: reattaching to deploys that outlived a restart,
// and loading the backup schedule.
func (s *Server) Start(ctx context.Context) {
	// Ask every daemon what it is, BEFORE serving a request.
	//
	// watchHosts reconciles on its sweep, but its first sweep is a minute away — and a minute is
	// long enough to tell somebody their Swarm manager is a standalone host, hide the Services
	// page from them, and have them believe it. What Daffa is pointed at is not a thing to be
	// approximately right about, and the answer costs one Info() call per node.
	s.ReconcileAll(ctx)

	s.ReapOrphanedRuns(ctx)

	// Every long-lived worker runs under a context Stop can cancel, and under the WaitGroup Stop
	// waits on — so a shutdown is coordinated rather than a race between process exit and workers
	// still looping. Deriving our own context (rather than only relying on the caller cancelling
	// theirs) means Stop can wind the workers down even on a crash-exit path.
	ctx, s.stopWorker = context.WithCancel(ctx)
	// Handlers that spawn their own long-lived work (an added SSH cluster's connect loop) parent
	// it on this, so it lives and dies with the worker set rather than with the request.
	s.workerCtx = ctx

	s.startScheduler(ctx)

	// The outbox drains outside any transaction — see notify/worker.go for why that is not
	// an optimisation but a correctness requirement.
	s.spawn(ctx, s.notify.Worker)
	s.spawn(ctx, s.watchHosts)

	// Sampling, and the partition maintenance that keeps it from filling the disk.
	s.spawn(ctx, s.collector.Run)
	s.spawn(ctx, s.retention.Run)

	// Deploy logs are the other thing here that grows forever if nobody sweeps it.
	s.spawn(ctx, s.pruneDeployments)

	// Certificate renewal, CA rotation warnings, and delivery reconciliation — the two
	// cron jobs from internal-setup, folded in. See cert_worker.go.
	s.spawn(ctx, s.certWorker)
	s.spawn(ctx, s.keyringWorker)

	// Dial out to every persisted SSH cluster and keep the connections up — the server-side
	// mirror of the agent connect loop (docs/clusters.md §4).
	s.spawn(ctx, s.watchSSHClusters)
}

// spawn runs a background worker under the WaitGroup, so Stop can wait for it to return.
func (s *Server) spawn(ctx context.Context, fn func(context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(ctx)
	}()
}

// workerStopGrace bounds how long Stop waits for the workers to drain. A worker wedged in a
// Docker call that ignores cancellation must not be able to hold the whole process open forever;
// past this, the shutdown proceeds and the OS reclaims what is left.
const workerStopGrace = 15 * time.Second

// Stop winds the background work down: it stops the scheduler, cancels the worker context, and
// waits (bounded) for every spawned worker to return. Safe to call once, after Start.
func (s *Server) Stop() {
	s.stopScheduler()
	if s.stopWorker != nil {
		s.stopWorker()
	}

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(workerStopGrace):
		slog.Warn("background workers did not stop within the grace period; exiting anyway")
	}
}

// route is one entry in the API's authorization table.
//
// A route must declare EITHER a capability OR a reason for being open. Nothing may declare
// both, and nothing may declare neither — TestEveryRouteIsGuarded enforces that. The point
// is that "open by choice" and "open because somebody forgot" stop looking alike: the
// second one fails the build, and the first one has to be justified in a string that a
// reviewer will read.
//
// A capability route must ALSO declare where it applies. Since grants can be scoped to a
// cluster, "may this person do X" is only half a question — the other half is "here?".
type route struct {
	pattern string
	cap     caps.Cap  // zero ⇒ see open
	scope   scopeKind // where the capability is checked
	open    string    // why this route needs no capability
	h       http.HandlerFunc

	// Generator metadata — see docs/openapi.md. Zero values of the payload types, so a
	// renamed or deleted type breaks the BUILD, not the docs. All three are runtime-inert;
	// nil/"" means "not yet declared", which the coverage ratchet tracks.
	req  any    // request body type, e.g. logConfigRequest{}
	resp any    // response type; a typed nil pointer, (*T)(nil), means "T or null"
	ts   string // generated daffa client method name; "" = the method stays in api-manual.ts
}

// scopeKind says how a route's environment is found. The zero value is scopeUnset, so a
// route that declares no scope fails the build rather than defaulting to something.
type scopeKind int

const (
	scopeUnset scopeKind = iota

	// scopeNone: the route is open; there is nothing to check.
	scopeNone

	// scopeGlobal: fleet-wide. Only a global grant satisfies it. Everything here is either
	// administrative (users, roles, settings) or brings a cluster into existence (agents),
	// neither of which means anything "on one cluster".
	scopeGlobal

	// scopeEnv: the environment is the {cluster} path value.
	scopeEnv

	// scopeStack: the {id} path value is a stack; its environment is the stack's. The
	// middleware resolves it and stashes the stack, so the handler's s.stack() does not
	// pay for the lookup twice.
	scopeStack

	// scopeJob: the {id} path value is a backup job; likewise.
	scopeJob

	// scopeDeployment: the {id} path value is a deployment. It has no cluster of its own — its
	// STACK does — so the middleware walks deployment → stack → cluster and checks there. A
	// deployment id is a global handle, and serving one without that walk would hand every
	// deploy log in the fleet to anyone who could guess an id.
	scopeDeployment

	// scopeMonitor: the {id} path value is a resource monitor. Its environment is the cluster it
	// watches — and a monitor with NO cluster watches the whole fleet, so it takes the capability
	// globally. See requireOnMonitor.
	scopeMonitor

	// scopeVolumeSource: the {id} path value is a volume source; its environment is the
	// cluster it delivers to. Same resolve-check-stash as a stack.
	scopeVolumeSource

	// scopeAny: satisfied by the capability held globally OR on any cluster. Only for the
	// three fleet-wide read lists (git credentials, registries, storage) — they have no
	// cluster, they carry no secrets, and an operator scoped to one cluster still has to
	// pick one when creating a stack there.
	scopeAny

	// scopeBody: the environment arrives in the request BODY, so no middleware can see it.
	// The HANDLER must check, after decoding. There are exactly two of these and a test
	// pins the list, so a third cannot be added without someone deciding to.
	scopeBody
)

// statusResponse is the API's plain acknowledgement: {"status": "ok"}. Deletes and other
// mutations with nothing better to say answer with it, so the shape is declared once.
type statusResponse struct {
	Status string `json:"status"`
}

var okStatus = statusResponse{Status: "ok"}

// testResult is the answer of every "does this actually work?" button — SMTP, an identity
// provider's discovery document, a chat webhook. The HTTP status is 200 either way; ok says
// whether the thing worked, and error carries the far end's own words when it did not.
type testResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// apiRoutes is the complete authenticated surface. Everything under /api/ is here, and the
// capability beside each line IS the authorization rule — there is no second place to look.
func (s *Server) apiRoutes() []route {
	return []route{
		// ── session ────────────────────────────────────────────────────────────────
		// The masks in caps/caps_by_env are one number per functional area; the browser
		// resolves a question as `global | byEnv[currentHost]` (see hasCap in caps.ts).
		//oapi:summary Read your own identity, roles and capability masks
		//oapi:enum MeResponse.kind local|oidc
		//oapi:enum MeRole.scope global|env
		{pattern: "GET /api/auth/me", scope: scopeNone, open: "a signed-in user may always read their own identity and permissions", h: s.handleMe,
			resp: meResponse{}},
		//oapi:summary End your own session
		//oapi:noreq
		{pattern: "POST /api/auth/logout", scope: scopeNone, open: "ending your own session is not a privileged act", h: s.handleLogout,
			resp: statusResponse{}, ts: "logout"},

		// ── API tokens ─────────────────────────────────────────────────────────────
		// Self-service is open for the same reason /me is: managing your own credential
		// is a property of being signed in, not a granted power. Token-authenticated
		// callers are refused in the handlers — a token that could mint tokens would
		// survive its own revocation. See docs/tokens.md.
		//oapi:summary List your own API tokens
		{pattern: "GET /api/tokens", scope: scopeNone, open: "your own credentials are yours to see; the response carries names and prefixes, never a secret", h: s.handleListMyTokens,
			resp: []tokenView(nil), ts: "myTokens"},
		// The response is the only time the secret ever exists outside the caller's
		// hands: only its hash is stored, so there is no second chance to read it.
		//oapi:summary Mint an API token for yourself; the secret is returned exactly once
		//oapi:example req {"name": "ci-deploy", "expires_days": 90}
		{pattern: "POST /api/tokens", scope: scopeNone, open: "minting a credential for yourself is not a granted power; the handler refuses token-authenticated callers", h: s.handleCreateToken,
			req: createTokenRequest{}, resp: createdTokenResponse{}, ts: "createToken"},
		//oapi:summary Revoke an API token
		{pattern: "DELETE /api/tokens/{id}", scope: scopeNone, open: "the handler enforces owner-or-users.edit, answering 404 either way", h: s.handleDeleteToken,
			resp: statusResponse{}, ts: "deleteToken"},
		// The users.edit oversight list: every token, with owners. Oversight without
		// impersonation — the secret is unrecoverable even here.
		//oapi:summary List every user's API tokens
		{pattern: "GET /api/tokens/all", cap: caps.UsersEdit, scope: scopeGlobal, h: s.handleListAllTokens,
			resp: []tokenView(nil), ts: "allTokens"},

		// ── capability registry ────────────────────────────────────────────────────
		// The role editor renders its matrix from this. Anyone who can see a role can
		// see what the bits in it mean.
		//oapi:summary Read the capability registry — the areas and capabilities, as data
		//oapi:enum Capability.mode view|edit|
		{pattern: "GET /api/capabilities", cap: caps.RolesView, scope: scopeGlobal, h: s.handleListCapabilities,
			resp: capabilitiesResponse{}, ts: "capabilities"},

		// The generated OpenAPI description of everything in this table. Also available
		// without a server via `daffa openapi`.
		//oapi:summary Read this API's OpenAPI 3.1 description
		{pattern: "GET /api/openapi.json", scope: scopeNone, open: "the spec describes shapes, not data; any signed-in user or token holder may read what using the API would reveal anyway", h: s.handleOpenAPISpec},

		// ── clusters ──────────────────────────────────────────────────────────────────
		// Listing clusters is clusters.view rather than open: without it the console has
		// no cluster to point at, so a role that omits it coherently sees nothing at all.
		// The list is filtered, not gated: each cluster appears only to a caller who
		// holds clusters.view there (or globally). Node status is probed live, per node.
		//oapi:summary List the clusters the caller may see, with live node status
		//oapi:enum Environment.status online|offline
		//oapi:enum Node.status online|offline
		//oapi:enum Node.kind local|agent|ssh
		{pattern: "GET /api/clusters", cap: caps.ClustersView, scope: scopeAny, h: s.handleListEnvironments,
			resp: []envView(nil), ts: "environments"},
		// Adding a cluster brings a whole environment into existence over SSH, so — like enrolling
		// an agent — it is clusters.edit taken globally, never a per-cluster grant. Test dials and
		// reads the daemon without persisting; create dials, pins the host key, and persists.
		//oapi:summary Test an SSH connection to a would-be cluster without saving it
		//oapi:example req {"host": "10.0.0.9", "user": "daffa", "key_id": "sshkey_…"}
		{pattern: "POST /api/clusters/test-connection", cap: caps.ClustersEdit, scope: scopeGlobal, h: s.handleTestSSHConnection,
			req: sshClusterRequest{}, resp: sshTestResponse{}, ts: "testClusterConnection"},
		//oapi:summary Add a cluster reached over SSH
		//oapi:example req {"name": "prod-eu", "host": "10.0.0.9", "user": "daffa", "key_id": "sshkey_…"}
		{pattern: "POST /api/clusters", cap: caps.ClustersEdit, scope: scopeGlobal, h: s.handleCreateSSHCluster,
			req: sshClusterRequest{}, resp: sshClusterCreatedResponse{}, ts: "createCluster"},
		// Installs Docker on a bare machine over SSH, streaming the setup log. Running a root script
		// on someone else's box is a strictly larger power than registering a connection, so it has
		// its own capability (docs/clusters.md §8, §14.3). SSE.
		//oapi:summary Install Docker on a machine over SSH (server-sent events)
		//oapi:produces text/event-stream
		//oapi:example req {"host": "10.0.0.9", "user": "root", "key_id": "sshkey_…"}
		{pattern: "POST /api/clusters/provision", cap: caps.ClustersProvision, scope: scopeGlobal, h: s.handleProvision,
			req: sshClusterRequest{}},
		// Removes an SSH cluster: stops its connection and deletes the environment. The local
		// cluster and agent-backed ones are refused (409) — those are removed by other means.
		//oapi:summary Remove a cluster added over SSH
		{pattern: "DELETE /api/clusters/{cluster}", cap: caps.ClustersEdit, scope: scopeGlobal, h: s.handleDeleteCluster,
			resp: map[string]string(nil), ts: "deleteCluster"},
		// Hand-written client (api-manual.ts): the ?node= arity rule the generator cannot express,
		// shared with containers/stats. Still spec-documented via resp.
		//oapi:summary Read the Docker daemon's summary for one node
		//oapi:query node string the target node id; required only when the cluster has more than one
		{pattern: "GET /api/clusters/{cluster}/info", cap: caps.ClustersView, scope: scopeEnv, h: s.handleEnvInfo,
			resp: dockerx.Info{}},
		//oapi:summary Read disk usage for every node of the cluster, split by what could reclaim it
		{pattern: "GET /api/clusters/{cluster}/df", cap: caps.ClustersView, scope: scopeEnv, h: s.handleDiskUsage,
			resp: []nodeDiskView(nil), ts: "df"},
		// Names the cluster the way an operator thinks of it. Refused (409) when another cluster
		// already answers to the name — two identical entries in the switcher is a way to
		// restart the wrong machine.
		//oapi:summary Rename a cluster
		//oapi:required RenameRequest.name
		//oapi:example req {"name": "prod-eu"}
		{pattern: "PATCH /api/clusters/{cluster}", cap: caps.ClustersEdit, scope: scopeGlobal, h: s.handleRenameEnvironment,
			req: renameRequest{}, resp: renameResponse{}},

		// A cluster's container log defaults. Env-scoped on purpose — the SRE who runs
		// staging sets staging's log rotation; the FLEET default is the /api/settings
		// trio below with the same capability taken globally.
		//oapi:summary Read this cluster's container log defaults
		{pattern: "GET /api/clusters/{cluster}/logging", cap: caps.LoggingView, scope: scopeEnv, h: s.handleGetEnvLogConfig,
			resp: envLogConfigResponse{}, ts: "hostLogConfig"},
		// Applied to services that do not declare their own logging:, at their next deploy.
		//oapi:summary Set this cluster's container log override
		//oapi:example req {"driver": "local", "opts": {"max-size": "20m"}}
		{pattern: "PUT /api/clusters/{cluster}/logging", cap: caps.LoggingEdit, scope: scopeEnv, h: s.handleSaveEnvLogConfig,
			req: logConfigRequest{}, resp: store.LogConfig{}, ts: "saveHostLogConfig"},
		// Deleting the override reverts the cluster to the fleet default. Idempotent.
		//oapi:summary Revert this cluster to the fleet's log defaults
		//oapi:status 204
		{pattern: "DELETE /api/clusters/{cluster}/logging", cap: caps.LoggingEdit, scope: scopeEnv, h: s.handleDeleteEnvLogConfig,
			ts: "clearHostLogConfig"},

		// Enrolling an agent adds a machine Daffa can reach, and the enrolment token is a
		// credential. That is clusters.edit, not clusters.view.
		//oapi:summary List enrolled agents and their connection state
		//oapi:enum Agent.status online|offline|pending
		{pattern: "GET /api/agents", cap: caps.ClustersEdit, scope: scopeGlobal, h: s.handleListAgents,
			resp: []agentView(nil), ts: "agents"},
		// Declares an agent for a cluster and mints its one-time join token. The token is in this
		// response and never anywhere else — it exists only in the operator's clipboard. When the
		// agent connects, Daffa joins its machine to the named cluster's Swarm (docs/clusters.md §5).
		// Adding a node is nodes.edit at the target cluster, like the SSH add-node path — the cluster
		// is in the BODY, so the handler checks it via s.mayUseEnv (scopeBody, pinned by a test).
		//oapi:summary Create an agent for a cluster and mint its one-time join token
		//oapi:required CreateAgentRequest.name
		//oapi:example req {"name": "prod-worker-2", "cluster": "env_…", "role": "worker"}
		{pattern: "POST /api/agents", cap: caps.NodesEdit, scope: scopeBody, h: s.handleCreateAgent,
			req: createAgentRequest{}, resp: newAgentResponse{}, ts: "createAgent"},
		// Cuts the tunnel first, then forgets the agent — and the environment with it, if
		// that node was the last one in it.
		//oapi:summary Delete an agent and disconnect its tunnel
		{pattern: "DELETE /api/agents/{id}", cap: caps.ClustersEdit, scope: scopeGlobal, h: s.handleDeleteAgent,
			resp: statusResponse{}, ts: "deleteAgent"},

		// ── containers ─────────────────────────────────────────────────────────────
		// Fans out across every node of a swarm; each row carries the node it lives on,
		// because a container id is unique per DAEMON, not per cluster.
		//oapi:summary List containers across every node of the cluster
		//oapi:query all boolean include stopped containers (default true; pass false for running only)
		{pattern: "GET /api/clusters/{cluster}/containers", cap: caps.ContainersView, scope: scopeEnv, h: s.handleListContainers,
			resp: []dockerx.Container(nil), ts: "containers"},
		// The response is Docker's own inspect object, passed through — its shape follows
		// the daemon's API version and is not stable enough to promise fields from.
		//oapi:summary Inspect a container (raw Docker inspect payload)
		//oapi:query node string the target node id; required only when the cluster has more than one
		{pattern: "GET /api/clusters/{cluster}/containers/{id}", cap: caps.ContainersView, scope: scopeEnv, h: s.handleInspectContainer,
			resp: map[string]any{}},
		// SSE. Each `log` event is one line; `end` marks a non-followed tail as complete,
		// `error` carries a mid-stream failure.
		//oapi:summary Stream a container's logs as server-sent events
		//oapi:enum LogLine.stream stdout|stderr
		//oapi:query tail integer how many lines of history to send first (default 200, max 10000)
		//oapi:query follow boolean keep the stream open for new lines (default true)
		//oapi:query node string the target node id; required only when the cluster has more than one
		//oapi:produces text/event-stream
		{pattern: "GET /api/clusters/{cluster}/containers/{id}/logs", cap: caps.ContainersView, scope: scopeEnv, h: s.handleContainerLogs,
			resp: dockerx.LogLine{}},
		// SSE. Follows ONE container — the one on screen — with a `stats` event per sample.
		//oapi:summary Stream one container's resource usage as server-sent events
		//oapi:query node string the target node id; required only when the cluster has more than one
		//oapi:produces text/event-stream
		{pattern: "GET /api/clusters/{cluster}/containers/{id}/stats", cap: caps.ContainersView, scope: scopeEnv, h: s.handleStatsStream,
			resp: dockerx.Stats{}},
		// One sample per named container. The CLIENT says which — it knows what is on
		// screen, and sampling a container nobody is looking at is work spent for nothing.
		//oapi:summary Sample resource usage for the named containers, once
		//oapi:query ids string comma-separated container ids to sample (at most 100)
		//oapi:query node string the target node id; required only when the cluster has more than one
		{pattern: "GET /api/clusters/{cluster}/stats", cap: caps.ContainersView, scope: scopeEnv, h: s.handleStatsSnapshot,
			resp: []dockerx.Stats(nil)},
		// SSE. Relays the daemon's container/image/volume/network events as `docker`
		// events, so the UI invalidates exactly what changed instead of polling.
		//oapi:summary Stream the Docker daemon's events as server-sent events
		//oapi:query node string the target node id; required only when the cluster has more than one
		//oapi:produces text/event-stream
		{pattern: "GET /api/clusters/{cluster}/events", cap: caps.ContainersView, scope: scopeEnv, h: s.handleEvents,
			resp: dockerEvent{}},
		//oapi:summary Run a lifecycle action on a container
		//oapi:path action enum=start|stop|restart|kill|pause|unpause|remove the lifecycle operation to perform
		//oapi:query force boolean remove only: also remove a running container
		//oapi:query node string the target node id; required only when the cluster has more than one
		//oapi:noreq
		{pattern: "POST /api/clusters/{cluster}/containers/{id}/{action}", cap: caps.ContainersEdit, scope: scopeEnv, h: s.handleContainerAction,
			resp: statusResponse{}},

		// Exec checks containers.exec INSIDE the handler, not here: a WebSocket upgrade is
		// a GET, so it cannot be distinguished from a read by the route pattern. It is
		// open at this layer and guarded at the next. See handleExec.
		//
		// The wire protocol is a WebSocket, not JSON: binary frames are raw terminal bytes
		// in both directions, text frames are control messages (resize). It cannot be
		// described as a JSON operation, so it is documented here in prose only.
		//oapi:summary Open an interactive shell in a container (WebSocket, not JSON)
		{pattern: "GET /api/clusters/{cluster}/containers/{id}/exec", scope: scopeNone, open: "a WebSocket upgrade is a GET; handleExec enforces containers.exec itself", h: s.handleExec},

		// ── images, networks, volumes ──────────────────────────────────────────────
		// Swarm. Cluster-wide, every one of them: these handlers reach their daemon through
		// s.control(), which is a manager, and never through s.node(). The node table is the
		// exception that proves it — it is a JOIN of what the swarm says exists against what
		// Daffa can reach, so it needs both, and it is gated on clusters.view because a node is
		// what a cluster is MADE of rather than a resource of its own.
		//oapi:summary List the cluster's services with live task counts
		//oapi:enum Service.mode replicated|global
		{pattern: "GET /api/clusters/{cluster}/services", cap: caps.ServicesView, scope: scopeEnv, h: s.handleListServices,
			resp: []dockerx.Service(nil), ts: "services"},
		//oapi:summary Read one service
		{pattern: "GET /api/clusters/{cluster}/services/{id}", cap: caps.ServicesView, scope: scopeEnv, h: s.handleInspectService,
			resp: dockerx.Service{}, ts: "service"},
		// The task is the point: a service saying 0/3 tells you nothing, the task's error
		// says why. Each row also says whether Daffa can reach the node it runs on.
		//oapi:summary List a service's tasks — the rows that say WHY a replica is not running
		{pattern: "GET /api/clusters/{cluster}/services/{id}/tasks", cap: caps.ServicesView, scope: scopeEnv, h: s.handleListTasks,
			resp: []taskView(nil), ts: "tasks"},
		// SSE. The one cluster-wide stream Docker proxies for us: the manager collects from
		// every node running a task, so it works with no agent on the workers at all.
		//oapi:summary Stream a service's logs from every node as server-sent events
		//oapi:query tail integer how many lines of history to send first (default 200)
		//oapi:query follow boolean keep the stream open for new lines (default false)
		//oapi:produces text/event-stream
		{pattern: "GET /api/clusters/{cluster}/services/{id}/logs", cap: caps.ServicesView, scope: scopeEnv, h: s.handleServiceLogs,
			resp: dockerx.LogLine{}},
		//oapi:summary List the cluster's machines: swarm membership joined against Daffa reachability
		//oapi:enum ClusterNode.role manager|worker
		{pattern: "GET /api/clusters/{cluster}/nodes", cap: caps.ClustersView, scope: scopeEnv, h: s.handleListNodes,
			resp: []clusterNodeView(nil), ts: "clusterNodes"},

		// `replicas` is a pointer on purpose: scaling to 0 is a real instruction, a missing
		// field is not, and only a nullable field can tell them apart.
		//oapi:summary Scale a service to an exact replica count
		//oapi:required ScaleRequest.replicas
		//oapi:example req {"replicas": 3}
		{pattern: "POST /api/clusters/{cluster}/services/{id}/scale", cap: caps.ServicesEdit, scope: scopeEnv, h: s.handleScaleService,
			req: scaleRequest{}, resp: statusResponse{}},
		// `docker service update --force`: recreate every task, re-resolving the image
		// against the registry — the only way to get new bytes for a floating tag without
		// editing anything.
		//oapi:summary Force-redeploy a service, re-resolving its image
		//oapi:noreq
		{pattern: "POST /api/clusters/{cluster}/services/{id}/redeploy", cap: caps.ServicesEdit, scope: scopeEnv, h: s.handleRedeployService,
			resp: statusResponse{}, ts: "redeployService"},
		// Puts back the service's PREVIOUS spec, which swarm keeps for exactly this. Rolls
		// back ONE service — a stack rollback re-applies a stored compose file and lives on
		// the deployment.
		//oapi:summary Roll a service back to its previous spec
		//oapi:noreq
		{pattern: "POST /api/clusters/{cluster}/services/{id}/rollback", cap: caps.ServicesEdit, scope: scopeEnv, h: s.handleRollbackService,
			resp: statusResponse{}, ts: "rollbackService"},
		//oapi:summary Remove a service from the cluster
		{pattern: "DELETE /api/clusters/{cluster}/services/{id}", cap: caps.ServicesEdit, scope: scopeEnv, h: s.handleRemoveService,
			resp: statusResponse{}, ts: "removeService"},

		// Node operations are their own capability. Draining a machine moves EVERYBODY's workload;
		// scaling one service moves one. An operator trusted with the second has not thereby been
		// trusted with the first.
		// Attaches a machine to THIS cluster's Swarm over SSH: Daffa reads the join token from the
		// manager and issues SwarmJoin itself, setting the new node's advertise address to its own
		// reachable host — nobody runs `docker swarm join` by hand (docs/clusters.md §5).
		//oapi:summary Add a node to this cluster's Swarm over SSH
		//oapi:example req {"host": "10.0.0.10", "user": "daffa", "key_id": "sshkey_…", "role": "worker"}
		{pattern: "POST /api/clusters/{cluster}/nodes", cap: caps.NodesEdit, scope: scopeEnv, h: s.handleAddNode,
			req: addNodeRequest{}, resp: addNodeResponse{}, ts: "addNode"},
		// Say what to change: an availability (active|pause|drain) or a role
		// (manager|worker) — one per request.
		//oapi:summary Change a machine's availability or role in the swarm
		//oapi:path id the SWARM node id, not Daffa's — the node table sends both
		//oapi:example req {"availability": "drain"}
		{pattern: "PATCH /api/clusters/{cluster}/nodes/{id}", cap: caps.NodesEdit, scope: scopeEnv, h: s.handleUpdateNode,
			req: nodeUpdateRequest{}, resp: statusResponse{}, ts: "updateNode"},
		// Removes the swarm's RECORD of a machine; it does not reach the machine itself
		// (`docker swarm leave` is what a node runs on itself). For the one already gone.
		//oapi:summary Remove a machine from the swarm's records
		//oapi:path id the SWARM node id, not Daffa's
		//oapi:query force boolean remove even if the swarm still believes the node is active
		{pattern: "DELETE /api/clusters/{cluster}/nodes/{id}", cap: caps.NodesEdit, scope: scopeEnv, h: s.handleRemoveNode,
			resp: statusResponse{}},

		// Swarm secrets and configs are retired: a secret is a STACK's sealed sub-resource now
		// (GET/PUT /api/stacks/{id}/secrets, docs/secrets.md), and config lives in git behind a
		// volume source (docs/volumes.md).

		// The cluster's own existence. The join tokens are a CREDENTIAL — anybody holding one can add
		// a machine to the cluster — so they are served by exactly one route, which requires
		// swarm.edit, and they appear in no other payload anywhere.
		//oapi:summary Turn a standalone host into a single-node Swarm
		//oapi:query node string the target node id; required only when the cluster has more than one
		//oapi:example req {"advertise_addr": "10.0.0.5"}
		{pattern: "POST /api/clusters/{cluster}/swarm/init", cap: caps.SwarmEdit, scope: scopeEnv, h: s.handleSwarmInit,
			req: swarmInitRequest{}, resp: swarmInitResponse{}},
		// Reading these is reading a credential, and it is audited as such.
		//oapi:summary Read the swarm's join tokens — the credentials that admit a machine
		{pattern: "GET /api/clusters/{cluster}/swarm/tokens", cap: caps.SwarmEdit, scope: scopeEnv, h: s.handleJoinTokens,
			resp: dockerx.JoinTokens{}, ts: "joinTokens"},
		// For the last manager this dissolves the cluster — the raft store goes, and with
		// it every service definition — which is why Docker demands force, and so do we.
		//oapi:summary Take a node out of its Swarm
		//oapi:query force boolean required for a manager; the last manager dissolves the cluster
		//oapi:query node string the target node id; required only when the cluster has more than one
		//oapi:noreq
		{pattern: "POST /api/clusters/{cluster}/swarm/leave", cap: caps.SwarmEdit, scope: scopeEnv, h: s.handleSwarmLeave,
			resp: statusResponse{}},

		// Fans out across every node; each row says whether a container (running or
		// stopped) still pins the image, because "can I delete this?" is the only question
		// anyone opens the list to answer.
		//oapi:summary List images across every node, biggest first, with in-use flags
		{pattern: "GET /api/clusters/{cluster}/images", cap: caps.ImagesView, scope: scopeEnv, h: s.handleListImages,
			resp: []dockerx.Image(nil), ts: "images"},
		//oapi:summary Remove an image from one node
		//oapi:query force boolean remove even if tagged in multiple repositories
		//oapi:query node string the target node id; required only when the cluster has more than one
		{pattern: "DELETE /api/clusters/{cluster}/images/{id}", cap: caps.ImagesEdit, scope: scopeEnv, h: s.handleRemoveImage,
			resp: statusResponse{}},
		// Fans out and deduplicates by id: an overlay network is cluster-wide and every
		// node reports it, but nobody needs to see it three times.
		//oapi:summary List networks across every node, overlays deduplicated
		{pattern: "GET /api/clusters/{cluster}/networks", cap: caps.NetworksView, scope: scopeEnv, h: s.handleListNetworks,
			resp: []dockerx.Network(nil), ts: "networks"},
		// bridge, host and none are Docker's own — system networks, marked as such in the
		// list and refused here with a 400 (code system_network), never forwarded to the
		// daemon.
		//oapi:summary Remove a network from one node
		//oapi:query node string the target node id; required only when the cluster has more than one
		{pattern: "DELETE /api/clusters/{cluster}/networks/{id}", cap: caps.NetworksEdit, scope: scopeEnv, h: s.handleRemoveNetwork,
			resp: statusResponse{}},
		// Each volume is annotated with the containers that mount it — the archaeology you
		// otherwise do by hand before daring to delete one. Orphans sort first.
		//oapi:summary List volumes across every node, with the containers that mount them
		{pattern: "GET /api/clusters/{cluster}/volumes", cap: caps.VolumesView, scope: scopeEnv, h: s.handleListVolumes,
			resp: []dockerx.Volume(nil), ts: "volumes"},
		// Refused (409) while anything Daffa manages depends on the volume — a volume
		// source that would rewrite it on the next sync, or a backup job that would fail
		// every night from then on.
		//oapi:summary Remove a volume from one node
		//oapi:query force boolean pass Docker's force flag through
		//oapi:query node string the target node id; required only when the cluster has more than one
		{pattern: "DELETE /api/clusters/{cluster}/volumes/{name}", cap: caps.VolumesEdit, scope: scopeEnv, h: s.handleRemoveVolume,
			resp: statusResponse{}},

		// Prune is host-wide, bulk and irreversible. Removing one image and sweeping every
		// unused one are not the same decision, so they are not the same permission.
		// `volumes` deletes ANONYMOUS unused volumes only — a named volume is data, removed
		// one at a time, on purpose, by a human.
		//oapi:summary Prune unused resources on one node, in bulk
		//oapi:path target enum=images|containers|networks|volumes|build-cache what to sweep
		//oapi:enum PruneResult.target images|containers|networks|volumes|build-cache
		//oapi:query node string the target node id; required only when the cluster has more than one
		//oapi:noreq
		{pattern: "POST /api/clusters/{cluster}/prune/{target}", cap: caps.SystemPrune, scope: scopeEnv, h: s.handlePrune,
			resp: dockerx.PruneResult{}, ts: "prune"},

		// ── stacks ─────────────────────────────────────────────────────────────────
		// The action buttons render FROM the actions list — the engines do not agree on
		// the verbs, and a hardcoded list would ship dead buttons.
		//oapi:summary List every stack the caller may view
		//oapi:enum Stack.actions up|pull|stop|down|restart|down+volumes
		//oapi:enum Stack.engine compose|swarm
		//oapi:enum Stack.source_kind git|inline
		{pattern: "GET /api/stacks", cap: caps.StacksView, scope: scopeAny, h: s.handleListStacks,
			resp: []stackView(nil), ts: "stacks"},
		// The source, the declared services, the live status and the swarm warnings, in one
		// answer. A broken source still returns the stack, with source_error saying why —
		// that is precisely when someone needs to open the page and fix it.
		//oapi:summary Read one stack: source, declared services, live status, warnings
		//oapi:enum StackStatus.state running|partial|stopped|not_deployed|unreachable
		{pattern: "GET /api/stacks/{id}", cap: caps.StacksView, scope: scopeStack, h: s.handleStackDetail,
			resp: stackDetailResponse{}, ts: "stack"},
		// The list rows carry no logs; the log streams from /api/deployments/{id}/logs.
		//oapi:summary List one stack's deployment history, newest first
		{pattern: "GET /api/stacks/{id}/deployments", cap: caps.StacksView, scope: scopeStack, h: s.handleListStackDeployments,
			resp: []deploymentView(nil), ts: "stackDeployments"},
		// stacks.view lists the KEYS; the handler reveals non-secret VALUES only to
		// stacks.edit, and secret ones to nobody. Field-level, so it cannot live here.
		//oapi:summary List a stack's env vars — values only for stacks.edit, secret values never
		{pattern: "GET /api/stacks/{id}/env", cap: caps.StacksView, scope: scopeStack, h: s.handleStackEnv,
			resp: []envVarView(nil), ts: "stackEnv"},
		// Stack secrets: names are listed to stacks.view; a secret's BYTES are never in the
		// response at all — it is write-only, sealed on the way in and read back by nobody.
		//oapi:summary List a stack's secret names — the bytes are write-only
		{pattern: "GET /api/stacks/{id}/secrets", cap: caps.StacksView, scope: scopeStack, h: s.handleStackSecrets,
			resp: []stackSecretView(nil), ts: "stackSecrets"},
		// The write-only rule's ONE gated exception (docs/secrets.md): reveal a single sealed value
		// on demand. secrets.reveal is standalone — stacks.edit does not imply it — and each reveal
		// is audited. scopeStack checks the cap at the stack's own env.
		//oapi:summary Reveal one secret env var's plaintext (audited; needs secrets.reveal)
		{pattern: "GET /api/stacks/{id}/env/{key}/reveal", cap: caps.SecretsReveal, scope: scopeStack, h: s.handleRevealStackEnv,
			resp: revealedValue{}, ts: "revealStackEnv"},
		//oapi:summary Reveal one secret file's plaintext (audited; needs secrets.reveal)
		{pattern: "GET /api/stacks/{id}/secrets/{name}/reveal", cap: caps.SecretsReveal, scope: scopeStack, h: s.handleRevealStackSecret,
			resp: revealedValue{}, ts: "revealStackSecret"},
		// Creating does not deploy: the stack is a record until its first `up`. On a
		// multi-node swarm a Compose stack must name node_id — the machine its containers
		// land on; a Swarm stack must not, because placement is the scheduler's.
		//oapi:summary Create a stack (recorded, not yet deployed)
		//oapi:example req {"env_id": "env_prod", "name": "blog", "engine": "compose", "source_kind": "git", "git_url": "https://github.com/acme/blog.git", "git_ref": "main", "git_path": "docker-compose.yml"}
		{pattern: "POST /api/stacks", cap: caps.StacksEdit, scope: scopeBody, h: s.handleCreateStack,
			req: stackRequest{}, resp: stackView{}, ts: "createStack"},
		// The engine is immutable: the containers it already made carry the old engine's
		// labels, and the new one would never find them. Create a new stack instead.
		//oapi:summary Update a stack's source (the engine cannot change)
		{pattern: "PUT /api/stacks/{id}", cap: caps.StacksEdit, scope: scopeStack, h: s.handleUpdateStack,
			req: stackRequest{}, resp: stackView{}, ts: "updateStack"},
		// Removes the stack's containers and network, then its record. Refused (409) while
		// a deploy runs, and when the teardown fails — a stack Daffa cannot clean up is one
		// it must not forget, unless ?force says to.
		//oapi:summary Delete a stack and what it deployed
		//oapi:query volumes boolean also remove the stack's named volumes — that destroys data, and takes volumes.edit on the host
		//oapi:query force boolean skip the teardown and only forget the stack; its containers keep running (for hosts that are gone)
		{pattern: "DELETE /api/stacks/{id}", cap: caps.StacksEdit, scope: scopeStack, h: s.handleDeleteStack,
			resp: statusResponse{}},
		// Replaces the whole set. A secret sent with an empty value means "unchanged" —
		// the client never had the plaintext to send back.
		//oapi:summary Replace a stack's environment variables
		{pattern: "PUT /api/stacks/{id}/env", cap: caps.StacksEdit, scope: scopeStack, h: s.handleSetStackEnv,
			req: setEnvRequest{}, resp: statusResponse{}},
		// Replaces the whole set. Empty content means "unchanged", same as env vars.
		//oapi:summary Replace a stack's secrets
		{pattern: "PUT /api/stacks/{id}/secrets", cap: caps.StacksEdit, scope: scopeStack, h: s.handleSetStackSecrets,
			req: setSecretsRequest{}, resp: statusResponse{}},
		// The webhook secret is in the response exactly once, when it is minted — on first
		// enable, or when rotate asks for a fresh one.
		//oapi:summary Turn auto-deploy on or off; the webhook secret is returned once
		//oapi:example req {"enabled": true, "watch_paths": "apps/web/**", "rotate": false}
		{pattern: "PUT /api/stacks/{id}/autodeploy", cap: caps.StacksEdit, scope: scopeStack, h: s.handleSetAutoDeploy,
			req: autoDeployRequest{}, resp: autoDeployResponse{}, ts: "setAutoDeploy"},
		// Which actions exist is the engine's business — the stack's `actions` list says
		// which of these this stack supports (swarm has no pull and no stop).
		//oapi:summary Run a stack action; answers with the deployment id to follow
		//oapi:path action enum=up|pull|stop|down|restart|down+volumes what to do to the stack
		//oapi:noreq
		{pattern: "POST /api/stacks/{id}/{action}", cap: caps.StacksEdit, scope: scopeStack, h: s.handleStackAction,
			resp: deployStartedResponse{}, ts: "stackAction"},

		// ── inline-compose image upgrades ────────────────────────────────────────────
		// The Images tab reads the YAML in the editor and helps bump tags. These are NOT
		// under /api/stacks/{id}/… on purpose: that would collide with the {id}/{action}
		// route above ("preview" would parse as a stack id). See .ai/image-upgrades.md.
		// All body-scoped (the env travels in the payload) — their handlers call mayUseEnv.
		//oapi:summary Parse a compose file into its unique images, classified for tag upgrades
		{pattern: "POST /api/compose/images", cap: caps.StacksView, scope: scopeBody, h: s.handlePreviewComposeImages,
			req: previewImagesRequest{}, resp: previewImagesResponse{}, ts: "previewComposeImages"},
		// Validation is the reliable path: one manifest read answers "does this tag exist".
		//oapi:summary Check whether an image tag exists in its registry
		{pattern: "POST /api/compose/tag-check", cap: caps.StacksView, scope: scopeBody, h: s.handleCheckImageTag,
			req: tagCheckRequest{}, resp: tagCheckResponse{}, ts: "checkImageTag"},
		// Best-effort "the newest looks like…" hint. Never fails, so it never blocks the row.
		//oapi:summary Suggest the newest tag that shares an image's version format
		{pattern: "POST /api/compose/latest-hint", cap: caps.StacksView, scope: scopeBody, h: s.handleLatestImageTag,
			req: latestHintRequest{}, resp: latestHintResponse{}, ts: "latestImageTag"},
		// Apply: swap the chosen tags into the YAML and hand it back. Rewriting the definition
		// is an edit, so unlike the reads above this one takes StacksEdit.
		//oapi:summary Rewrite image tags in a compose file, returning the updated YAML
		{pattern: "POST /api/compose/rewrite", cap: caps.StacksEdit, scope: scopeBody, h: s.handleRewriteComposeImages,
			req: rewriteRequest{}, resp: rewriteResponse{}, ts: "rewriteComposeImages"},

		// ── volume sources ─────────────────────────────────────────────────────────
		// Config from git into named volumes. See docs/volumes.md. The list is scopeAny
		// with visible() filtering, the stacks precedent; create is body-scoped (the env
		// arrives in the payload) and its handler calls mayUseEnv — the pinned list.
		//oapi:summary List every volume source the caller may view
		//oapi:enum VolumeSource.status pending|ok|error
		{pattern: "GET /api/volume-sources", cap: caps.VolSourcesView, scope: scopeAny, h: s.handleListVolumeSources,
			resp: []volumeSourceView(nil), ts: "volumeSources"},
		// The first sync starts in the background; the source row records its outcome. The
		// webhook secret (when auto_sync mints one) is in the response exactly once.
		//oapi:summary Create a volume source; the first sync starts in the background
		//oapi:example req {"env_id": "env_prod", "volume": "proxy-config", "git_url": "https://github.com/acme/config.git", "git_ref": "main", "git_path": "proxy", "auto_sync": true}
		{pattern: "POST /api/volume-sources", cap: caps.VolSourcesEdit, scope: scopeBody, h: s.handleCreateVolumeSource,
			req: volumeSourceRequest{}, resp: volumeSourceSavedResponse{}, ts: "createVolumeSource"},
		//oapi:summary Read one volume source
		{pattern: "GET /api/volume-sources/{id}", cap: caps.VolSourcesView, scope: scopeVolumeSource, h: s.handleGetVolumeSource,
			resp: volumeSourceView{}, ts: "volumeSource"},
		// env_id and volume are immutable — retargeting a source would strand the old
		// volume with a manifest nothing owns. Delete and recreate, so both halves are
		// explicit; the request's env/volume fields are ignored here.
		//oapi:summary Update a volume source (its host and volume cannot change)
		{pattern: "PUT /api/volume-sources/{id}", cap: caps.VolSourcesEdit, scope: scopeVolumeSource, h: s.handleUpdateVolumeSource,
			req: volumeSourceRequest{}, resp: volumeSourceSavedResponse{}, ts: "updateVolumeSource"},
		// The volume and its contents are left in place — it becomes an ordinary volume,
		// removable through volumes.edit when the operator decides.
		//oapi:summary Delete a volume source, leaving the volume and its contents alone
		{pattern: "DELETE /api/volume-sources/{id}", cap: caps.VolSourcesEdit, scope: scopeVolumeSource, h: s.handleDeleteVolumeSource,
			resp: statusResponse{}, ts: "deleteVolumeSource"},
		// Synchronous, and forced: "sync now" is the button an operator presses while
		// looking at a red status, and it answers with the outcome, not with "started".
		//oapi:summary Sync a volume source now; answers with the outcome
		//oapi:noreq
		{pattern: "POST /api/volume-sources/{id}/sync", cap: caps.VolSourcesEdit, scope: scopeVolumeSource, h: s.handleSyncVolumeSource,
			resp: volumeSourceView{}, ts: "syncVolumeSource"},

		// ── deployments ────────────────────────────────────────────────────────────
		//
		// Addressed by their own id rather than nested under their stack, so a deployment has
		// ONE canonical URL — the thing you can paste into a message. The cross-stack feed and
		// the per-stack history both link to the same page.
		//
		// No new capability: reading a deploy log is reading the stack, and rolling one back or
		// killing one is changing the stack. Inventing bits for them would imply an authority
		// that can roll back but not deploy, which is not a thing anyone wants.
		//oapi:summary List recent deployments across every stack, newest first
		//oapi:enum Deployment.status running|ok|failed|cancelled
		//oapi:enum Deployment.trigger_kind manual|webhook|rollback
		//oapi:query status enum=running|ok|failed|cancelled string only deployments with this status
		//oapi:query stack string only this stack's deployments (a stack id)
		//oapi:query host string only stacks on this host (an environment id)
		//oapi:query trigger enum=manual|webhook|rollback string only deployments started this way
		//oapi:query before string only deployments started before this RFC 3339 timestamp (for paging)
		{pattern: "GET /api/deployments", cap: caps.StacksView, scope: scopeAny, h: s.handleRecentDeployments,
			resp: []deploymentView(nil)},
		// The page you can send to somebody. Its log is not in here — it comes over SSE
		// from the logs route, which serves a live deploy and a finished one alike.
		//oapi:summary Read one deployment
		{pattern: "GET /api/deployments/{id}", cap: caps.StacksView, scope: scopeDeployment, h: s.handleDeploymentDetail,
			resp: deploymentView{}, ts: "deployment"},
		// Streams the runner's output while the deploy runs and replays the recorded log
		// once it is over, so the same URL works during the deploy and a week later.
		// Events: `log` {text, replace?}, `end` {status, exit_code}, `error` {message}.
		//oapi:summary Follow a deployment's log over SSE, live or replayed
		//oapi:produces text/event-stream
		{pattern: "GET /api/deployments/{id}/logs", cap: caps.StacksView, scope: scopeDeployment, h: s.handleDeploymentLogs},
		// Kills a deploy that is not going to finish. It does not undo what it already did;
		// the answer is {"status": "cancelling"} and the watcher records the final state.
		//oapi:summary Cancel a running deployment
		//oapi:noreq
		{pattern: "POST /api/deployments/{id}/cancel", cap: caps.StacksEdit, scope: scopeDeployment, h: s.handleCancelDeployment,
			resp: statusResponse{}, ts: "cancelDeployment"},
		// Re-applies the compose file STORED ON THAT DEPLOYMENT — it does not go back to
		// git, so a moved branch or an unreachable repo cannot stop you restoring what
		// worked. Only a succeeded up/pull whose file Daffa still has (`redeployable`).
		//oapi:summary Put an earlier deployment back
		//oapi:noreq
		{pattern: "POST /api/deployments/{id}/redeploy", cap: caps.StacksEdit, scope: scopeDeployment, h: s.handleRedeployDeployment,
			resp: deployStartedResponse{}, ts: "redeploy"},

		// ── backups ────────────────────────────────────────────────────────────────
		// Nothing sealed is ever in the list: not the S3 secret, not the DB password. The
		// encryption KEYS appear by name deliberately — an operator needs to know which
		// key a restore will demand.
		//oapi:summary List every backup job the caller may view
		//oapi:enum BackupJob.engine postgres|mysql|mongodb|volume
		//oapi:enum BackupJob.encryption age|none
		//oapi:enum BackupRun.status running|ok|failed
		//oapi:enum BackupRun.trigger manual|schedule
		{pattern: "GET /api/backups", cap: caps.BackupsView, scope: scopeAny, h: s.handleListBackupJobs,
			resp: []jobView(nil), ts: "backups"},
		//oapi:summary List a job's recent runs, newest first
		{pattern: "GET /api/backups/{id}/runs", cap: caps.BackupsView, scope: scopeJob, h: s.handleBackupRuns,
			resp: []*runView2(nil), ts: "backupRuns"},
		// What is actually in the bucket — the only honest answer to "do I have a
		// backup?", since a run record only says what Daffa believes it did.
		//oapi:summary List the snapshots actually in the job's bucket
		{pattern: "GET /api/backups/{id}/snapshots", cap: caps.BackupsView, scope: scopeJob, h: s.handleSnapshots,
			resp: []backups.Snapshot(nil), ts: "snapshots"},
		// The DB password is sealed on the way in and write-only from then on. Encryption
		// references named keys (see /api/keys), never raw age recipients.
		//oapi:summary Create a backup job
		//oapi:example req {"env_id": "env_prod", "name": "app-db-nightly", "container": "app-db",
		//  "engine": "postgres", "databases": "app", "db_user": "app", "db_password": "…",
		//  "schedule": "0 3 * * *", "storage_id": "st_1", "prefix": "app", "encryption": "age",
		//  "key_ids": ["key_1"]}
		{pattern: "POST /api/backups", cap: caps.BackupsEdit, scope: scopeBody, h: s.handleCreateBackupJob,
			req: jobRequest{}, resp: map[string]string(nil), ts: "createBackup"},
		// Deleting a job stops FUTURE backups. The snapshots already in the bucket are the
		// whole point, and Daffa will not touch them.
		//oapi:summary Delete a backup job, leaving its snapshots in the bucket
		{pattern: "DELETE /api/backups/{id}", cap: caps.BackupsEdit, scope: scopeJob, h: s.handleDeleteBackupJob,
			resp: map[string]string(nil), ts: "deleteBackup"},
		//oapi:summary Flip a job's schedule on or off
		//oapi:noreq
		{pattern: "POST /api/backups/{id}/toggle", cap: caps.BackupsEdit, scope: scopeJob, h: s.handleToggleBackupJob,
			resp: map[string]bool(nil), ts: "toggleBackup"},
		// Answers 202 and runs in the background: a dump can take hours, and an HTTP
		// request is not a place to wait for one.
		//oapi:summary Start a backup run now, without waiting for the schedule
		//oapi:status 202
		//oapi:noreq
		{pattern: "POST /api/backups/{id}/run", cap: caps.BackupsEdit, scope: scopeJob, h: s.handleRunBackup,
			resp: statusResponse{}, ts: "runBackup"},
		// A snapshot is the encrypted dump of an entire database, and a restore overwrites
		// a live one. Neither is implied by "can manage backup jobs".
		//
		// The download streams the snapshot back still encrypted (application/octet-stream):
		// the server holds no age key and cannot decrypt what it stores — decryption happens
		// in the CLI on the operator's machine.
		//oapi:summary Download a snapshot, still encrypted
		//oapi:query key string the snapshot's object key, from the snapshots list
		//oapi:produces application/octet-stream
		{pattern: "GET /api/backups/{id}/download", cap: caps.BackupsDownload, scope: scopeJob, h: s.handleSnapshotDownload},
		// The restore takes the ALREADY-DECRYPTED dump as the raw request body — the CLI
		// decrypts on the operator's machine and streams the plaintext here, because the
		// server is the only thing that can reach the container and the key stays with the
		// person. Overwrites the live database, so the job's name must be echoed back.
		//oapi:summary Restore a snapshot by streaming a decrypted dump as the request body
		//oapi:query confirm string the job's name, echoed back — restoring overwrites the live database
		{pattern: "POST /api/backups/{id}/restore", cap: caps.BackupsRestore, scope: scopeJob, h: s.handleRestore,
			resp: map[string]string(nil)},

		// ── storage, registries, git credentials ───────────────────────────────────
		// The list endpoints carry names and kinds, never secrets — an operator picks a
		// target when creating a job, so they must be able to see the list.
		//oapi:summary List storage targets — endpoints, buckets and key ids, never the secret
		{pattern: "GET /api/storage", cap: caps.StorageView, scope: scopeAny, h: s.handleListStorage,
			resp: []storageView(nil), ts: "storage"},
		// The bucket is proved reachable BEFORE it is saved — an unreachable one is not a
		// configuration, it is a future 3am surprise. The secret is sealed and write-only
		// from then on.
		//oapi:summary Create a storage target, testing the bucket first
		//oapi:example req {"name": "wasabi", "endpoint": "https://s3.eu-central-1.wasabisys.com",
		//  "region": "eu-central-1", "bucket": "daffa-backups", "key_id": "AKIA…", "secret": "…"}
		{pattern: "POST /api/storage", cap: caps.StorageEdit, scope: scopeGlobal, h: s.handleCreateStorage,
			req: storageRequest{}, resp: map[string]string(nil), ts: "createStorage"},
		// An empty secret means "keep the current one" — the UI cannot show it back, so
		// requiring it on every edit would mean re-typing a credential to change a bucket
		// name, and people would paste the wrong one.
		//oapi:summary Update a storage target; an empty secret keeps the current one
		{pattern: "PUT /api/storage/{id}", cap: caps.StorageEdit, scope: scopeGlobal, h: s.handleUpdateStorage,
			req: storageRequest{}, resp: map[string]string(nil), ts: "updateStorage"},
		// Refused while backup jobs still point at it — a job whose bucket vanished fails
		// at its next run, the worst possible moment to learn about it.
		//oapi:summary Delete a storage target no backup job uses
		{pattern: "DELETE /api/storage/{id}", cap: caps.StorageEdit, scope: scopeGlobal, h: s.handleDeleteStorage,
			resp: map[string]string(nil), ts: "deleteStorage"},

		//oapi:summary List registry credentials — URLs and usernames, never the password
		{pattern: "GET /api/registries", cap: caps.RegistriesView, scope: scopeAny, h: s.handleListRegistries,
			resp: []registryView(nil), ts: "registries"},
		// The credential is proved against the registry before it is saved; the password
		// is sealed and never readable again, not even by the admin who typed it.
		//oapi:summary Create a registry credential, testing the login first (advisory — save-anyway on unreachable)
		//oapi:example req {"name": "ghcr", "url": "ghcr.io", "username": "octocat", "password": "…"}
		{pattern: "POST /api/registries", cap: caps.RegistriesEdit, scope: scopeGlobal, h: s.handleCreateRegistry,
			req: registryRequest{}, resp: registryCreateResponse{}, ts: "createRegistry"},
		//oapi:summary Delete a registry credential
		{pattern: "DELETE /api/registries/{id}", cap: caps.RegistriesEdit, scope: scopeGlobal, h: s.handleDeleteRegistry,
			resp: map[string]string(nil), ts: "deleteRegistry"},

		//oapi:summary List git credentials — kinds and usernames, never a token or key
		//oapi:enum GitCredential.kind token|ssh
		{pattern: "GET /api/gitcreds", cap: caps.GitCredsView, scope: scopeAny, h: s.handleListGitCredentials,
			resp: []gitCredView(nil), ts: "gitCredentials"},
		// Editing takes GitCredsEdit: it makes the server open an outbound SSH connection, so it is
		// not a read anyone with a login should be able to trigger. verified is true only for hosts
		// whose keys come from an authenticated endpoint (github.com); the rest are trust-on-first-use.
		//oapi:summary Fetch a git host's SSH keys so the credential form can pin them
		//oapi:query host string the git host to scan, e.g. gitlab.example.com
		{pattern: "GET /api/gitcreds/host-keys", cap: caps.GitCredsEdit, scope: scopeGlobal, h: s.handleDiscoverHostKeys,
			resp: hostKeysResponse{}},
		// The token or private key is sealed on arrival and never leaves the server again.
		// An SSH key is parsed (with its passphrase) before it is accepted, so a mangled
		// paste fails while the key is still in the clipboard.
		//oapi:summary Create a git credential — an access token or an SSH deploy key
		//oapi:example req {"name": "GitHub deploy", "kind": "token", "username": "octocat", "token": "…"}
		{pattern: "POST /api/gitcreds", cap: caps.GitCredsEdit, scope: scopeGlobal, h: s.handleCreateGitCredential,
			req: gitCredRequest{}, resp: map[string]string(nil), ts: "createGitCredential"},
		// Reaches out with the sealed credential (ls-remote), so it is GitCredsEdit like create.
		// The result is a diagnostic payload, not an API status: a failed test is still a 200.
		//oapi:summary Test a git credential against a repository URL (ls-remote, no clone)
		//oapi:example req {"url": "https://git.example.com/me/repo.git"}
		{pattern: "POST /api/gitcreds/{id}/test", cap: caps.GitCredsEdit, scope: scopeGlobal, h: s.handleTestGitCredential,
			req: gitTestRequest{}, resp: gitTestResponse{}, ts: "testGitCredential"},
		// Refused while stacks still use it — a stack whose credential vanished fails at
		// its next deploy, which is a bad time to find out.
		//oapi:summary Delete a git credential no stack uses
		{pattern: "DELETE /api/gitcreds/{id}", cap: caps.GitCredsEdit, scope: scopeGlobal, h: s.handleDeleteGitCredential,
			resp: map[string]string(nil), ts: "deleteGitCredential"},

		// ── ssh keys ───────────────────────────────────────────────────────────────
		// The credential-store pattern again: view is env-grantable and secret-free (names,
		// fingerprints and the PUBLIC key, via scopeAny), edit is global-only, because holding
		// the sealed private half — the thing Daffa dials out with — is a fleet-wide power.
		//oapi:summary List SSH keys — names, algorithms, fingerprints and public keys, never a private key
		//oapi:enum SSHKey.algo ed25519|rsa|ecdsa
		{pattern: "GET /api/ssh-keys", cap: caps.SSHKeysView, scope: scopeAny, h: s.handleListSSHKeys,
			resp: []sshKeyView(nil), ts: "sshKeys"},
		// Generate a fresh keypair or import an existing private key. Either way the private
		// half is sealed on arrival and never returned; the response carries the PUBLIC key so
		// the operator can paste it into the target's authorized_keys straight away.
		//oapi:summary Create an SSH key — generate a keypair or import a private key
		//oapi:example req {"name": "prod-fleet", "mode": "generate", "algo": "ed25519"}
		{pattern: "POST /api/ssh-keys", cap: caps.SSHKeysEdit, scope: scopeGlobal, h: s.handleCreateSSHKey,
			req: sshKeyRequest{}, resp: sshKeyCreateResponse{}, ts: "createSSHKey"},
		// Refused while a cluster or node still authenticates with it — that reference is added
		// in a later phase; today nothing does, so the guard is wired but always passes.
		//oapi:summary Delete an SSH key nothing uses
		{pattern: "DELETE /api/ssh-keys/{id}", cap: caps.SSHKeysEdit, scope: scopeGlobal, h: s.handleDeleteSSHKey,
			resp: map[string]string(nil), ts: "deleteSSHKey"},

		// ── certificates & encryption keys ─────────────────────────────────────────
		// The same shape as the other credential stores: view is env-grantable and lists
		// names, SANs and expiry (never key material) via scopeAny; edit is global-only,
		// because a CA Daffa signs with is fleet trust, not a host setting — so the
		// mutating routes need no per-env check at all.
		//oapi:summary List certificate authorities — subjects and expiry, never key material
		//oapi:enum CertAuthority.status active|next|retired
		{pattern: "GET /api/certs/cas", cap: caps.CertsView, scope: scopeAny, h: s.handleListCAs,
			resp: []caView(nil), ts: "cas"},
		// Generate a fresh root (cert_pem absent), or upload an existing one. A cert
		// uploaded WITHOUT its key is accepted deliberately: a trust-only anchor Daffa can
		// bundle and deliver but never sign with. An uploaded key is sealed at rest.
		//oapi:summary Create a CA — generate a root, or upload an existing one
		//oapi:example req {"name": "internal-ca", "common_name": "Acme Internal CA", "org": "Acme"}
		{pattern: "POST /api/certs/cas", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleCreateCA,
			req: caRequest{}, resp: caView{}, ts: "createCA"},
		// PHASE 1 of the two-phase rotation (docs/certs.md): stage a successor alongside
		// the incumbent. Nothing is re-signed and nothing can break — the new root simply
		// starts appearing in the trust bundle so distribution can begin.
		//oapi:summary Stage a successor root beside an active CA
		//oapi:example req {"overlap_days": 30}
		{pattern: "POST /api/certs/cas/{id}/rotate", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleRotateCA,
			req: caRotateRequest{}, resp: caView{}, ts: "rotateCA"},
		// PHASE 2: promote the staged successor and re-sign every leaf of the old root.
		// This is the step that breaks anything that never installed the new root, so it
		// demands confirm: true and never fires on a timer.
		//oapi:summary Activate a staged successor, re-signing every leaf of the old root
		//oapi:example req {"confirm": true}
		{pattern: "POST /api/certs/cas/{id}/activate", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleActivateCA,
			req: caActivateRequest{}, resp: caView{}},
		// Refused while certificates it signed still exist — a leaf whose CA vanished can
		// never renew again, and the day you find out is the day it expires.
		//oapi:summary Delete a CA that signed nothing still tracked
		{pattern: "DELETE /api/certs/cas/{id}", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleDeleteCA,
			resp: map[string]string(nil), ts: "deleteCA"},
		// The bundle is public key material, but it is served like everything else —
		// machines get it via deliveries, humans via the UI, nobody anonymously. A PEM
		// file, not JSON, so no client method is generated for it.
		//oapi:summary Download the trust bundle — every root a client should currently trust, as PEM
		//oapi:produces application/x-pem-file
		{pattern: "GET /api/certs/bundle", cap: caps.CertsView, scope: scopeAny, h: s.handleTrustBundle},

		//oapi:summary List certificates — names, SANs and expiry, never the private key
		//oapi:enum Certificate.status ok|error
		{pattern: "GET /api/certs", cap: caps.CertsView, scope: scopeAny, h: s.handleListCertificates,
			resp: []certView(nil), ts: "certs"},
		// Issue from a signing CA (ca_id + sans), or upload a cert_pem/chain_pem/key_pem
		// set. An uploaded pair is tracked, delivered and alerted on — but only its owner
		// can renew it. Private keys are sealed at rest and never served back.
		//oapi:summary Create a certificate — issue from a CA, or upload an existing pair
		//oapi:example req {"name": "web", "ca_id": "ca_1", "sans": ["web.internal", "10.0.0.5"]}
		{pattern: "POST /api/certs", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleCreateCertificate,
			req: certRequest{}, resp: certView{}, ts: "createCert"},
		// Edits what is editable. For an ISSUED certificate that includes its SANs — the
		// edit re-issues immediately with the same key; for an UPLOADED one a fresh
		// cert_pem/key_pem pair is renewal by re-upload. The name is immutable: it is the
		// filename deliveries have already written into volumes.
		//oapi:summary Update a certificate — SANs, renewal window, or a re-uploaded pair
		{pattern: "PUT /api/certs/{id}", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleUpdateCertificate,
			req: certRequest{}, resp: certView{}, ts: "updateCert"},
		// Renew now, without waiting for the renewal window. rotate_key also generates a
		// fresh private key — the deliberate version of what renewal deliberately avoids.
		//oapi:summary Renew an issued certificate now
		{pattern: "POST /api/certs/{id}/renew", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleRenewCertificate,
			req: certRenewRequest{}, resp: certView{}},
		// Refused while deliveries still carry it.
		//oapi:summary Delete a certificate no delivery carries
		{pattern: "DELETE /api/certs/{id}", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleDeleteCertificate,
			resp: map[string]string(nil), ts: "deleteCert"},

		// A delivery keeps certificate material current inside a named volume on a host,
		// where a container mounts it read-only. Private keys land 0600; the trust bundle
		// rides along in every volume.
		//oapi:summary List cert deliveries and their sync state
		//oapi:enum CertDelivery.status pending|ok|error
		{pattern: "GET /api/certs/deliveries", cap: caps.CertsView, scope: scopeAny, h: s.handleListCertDeliveries,
			resp: []deliveryView(nil), ts: "certDeliveries"},
		// The first sync runs in the background right away — creating a delivery should
		// not hang the request on a volume write, but it should go green within seconds.
		//oapi:summary Create a delivery of a certificate into a volume
		//oapi:example req {"env_id": "env_prod", "cert_id": "crt_1", "volume": "daffa-certs", "traefik": true}
		{pattern: "POST /api/certs/deliveries", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleCreateCertDelivery,
			req: certDeliveryRequest{}, resp: deliveryView{}, ts: "createCertDelivery"},
		// Synchronous, and forced: "sync now" is the button an operator presses while
		// looking at a red status, and it answers with the outcome, not with "started".
		//oapi:summary Sync a delivery now and answer with the outcome
		//oapi:noreq
		{pattern: "POST /api/certs/deliveries/{id}/sync", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleSyncCertDelivery,
			resp: deliveryView{}, ts: "syncCertDelivery"},
		// The volume and its contents are left in place: the consumer may still be serving
		// with them, and deleting key material out from under a running proxy is a worse
		// surprise than a stale file.
		//oapi:summary Delete a delivery, leaving the volume's files in place
		{pattern: "DELETE /api/certs/deliveries/{id}", cap: caps.CertsEdit, scope: scopeGlobal, h: s.handleDeleteCertDelivery,
			resp: map[string]string(nil), ts: "deleteCertDelivery"},

		// Backup encryption keys. The one response in Daffa that ever carries a private
		// key is the generate response, once — see handleCreateKey.
		//oapi:summary List encryption keys — public age recipients only
		//oapi:enum EncryptionKey.source generated|imported
		{pattern: "GET /api/keys", cap: caps.KeysView, scope: scopeAny, h: s.handleListKeys,
			resp: []keyView(nil), ts: "encryptionKeys"},
		// Generate an age keypair (recipient absent), or import a public recipient — a
		// pasted PRIVATE key is refused. Generation returns identity_file exactly once:
		// created in memory, never stored, download it or lose it. The box cannot read
		// its own backups, and that is the point.
		//oapi:summary Create an encryption key; generation returns the private half exactly once
		//oapi:example req {"name": "backups-2026"}
		{pattern: "POST /api/keys", cap: caps.KeysEdit, scope: scopeGlobal, h: s.handleCreateKey,
			req: keyRequest{}, resp: createdKeyResponse{}, ts: "createKey"},
		// Refused while backup jobs still encrypt to it: removing a recipient silently
		// narrows who can restore, and the day that matters is the worst possible day.
		//oapi:summary Delete an encryption key no backup job encrypts to
		{pattern: "DELETE /api/keys/{id}", cap: caps.KeysEdit, scope: scopeGlobal, h: s.handleDeleteKey,
			resp: map[string]string(nil), ts: "deleteKey"},

		// ── keyrings ───────────────────────────────────────────────────────────────
		// The credential-store split once more: one fleet-shared list, view env-grantable
		// and secret-free (a version is a kid, a state and an age — the material never
		// leaves a delivery volume), edit global-only because rotating or retiring changes
		// what every consumer on every host can decrypt. See docs/keyrings.md.
		//oapi:summary List keyrings and their version timelines — kids, states and ages, never material
		//oapi:enum KeyringVersion.state active|decrypt_only|retired
		{pattern: "GET /api/keyrings", cap: caps.KeyringsView, scope: scopeAny, h: s.handleListKeyrings,
			resp: []keyringView(nil), ts: "keyrings"},
		// The first version is minted and sealed immediately — a keyring with nothing to
		// encrypt with is an invalid state, and every delivery of it would fail.
		//oapi:summary Create a keyring, seeded with its first active version
		//oapi:example req {"name": "app-secrets", "rotate_days": 90}
		{pattern: "POST /api/keyrings", cap: caps.KeyringsEdit, scope: scopeGlobal, h: s.handleCreateKeyring,
			req: keyringRequest{}, resp: keyringView{}, ts: "createKeyring"},
		// Only the schedule is editable. The name is the filename deliveries write, and a
		// rename would leave every volume holding a stale file that looks current.
		//oapi:summary Set a keyring's rotation schedule (0 = manual only)
		{pattern: "PUT /api/keyrings/{id}", cap: caps.KeyringsEdit, scope: scopeGlobal, h: s.handleUpdateKeyring,
			req: keyringUpdateRequest{}, resp: keyringView{}, ts: "updateKeyring"},
		// The previous active version drops to decrypt_only and every delivery resyncs —
		// consumers see the new version within seconds.
		//oapi:summary Rotate a keyring — mint, seal and activate a new version
		//oapi:noreq
		{pattern: "POST /api/keyrings/{id}/rotate", cap: caps.KeyringsEdit, scope: scopeGlobal, h: s.handleRotateKeyring,
			resp: keyringView{}, ts: "rotateKeyring"},
		// Retirement IS the version vanishing from every delivered volume at the next
		// sync: data encrypted under it stops being decryptable there. Only a decrypt_only
		// version can retire — the active one must be rotated away first.
		//oapi:summary Retire a decrypt-only version, dropping it from every delivery
		//oapi:noreq
		{pattern: "POST /api/keyrings/{id}/versions/{vid}/retire", cap: caps.KeyringsEdit, scope: scopeGlobal, h: s.handleRetireKeyringVersion,
			resp: keyringView{}, ts: "retireKeyringVersion"},
		// Refused while deliveries still carry it. The sealed rows here are the ONLY
		// durable copy of the material — the delivered volumes keep their last-written
		// files, but they will never rotate again.
		//oapi:summary Delete a keyring no delivery carries
		{pattern: "DELETE /api/keyrings/{id}", cap: caps.KeyringsEdit, scope: scopeGlobal, h: s.handleDeleteKeyring,
			resp: map[string]string(nil), ts: "deleteKeyring"},

		// The cert-delivery machinery with a different payload: <name>.json (every live
		// version, and which is current) and <name>.current.key (the active version's raw
		// bytes), 0600, inside a named volume the consumer mounts read-only.
		//oapi:summary List keyring deliveries and their sync state
		//oapi:enum KeyringDelivery.status pending|ok|error
		{pattern: "GET /api/keyrings/deliveries", cap: caps.KeyringsView, scope: scopeAny, h: s.handleListKeyringDeliveries,
			resp: []keyringDeliveryView(nil), ts: "keyringDeliveries"},
		// The first sync runs in the background right away — creating a delivery should
		// not hang the request on a volume write, but it should go green within seconds.
		//oapi:summary Create a delivery of a keyring into a volume
		//oapi:example req {"keyring_id": "kr_1", "env_id": "env_prod", "volume": "daffa-keys"}
		{pattern: "POST /api/keyrings/deliveries", cap: caps.KeyringsEdit, scope: scopeGlobal, h: s.handleCreateKeyringDelivery,
			req: keyringDeliveryRequest{}, resp: keyringDeliveryView{}, ts: "createKeyringDelivery"},
		// Synchronous, and forced: "sync now" is the button an operator presses while
		// looking at a red status, and it answers with the outcome, not with "started".
		//oapi:summary Sync a delivery now and answer with the outcome
		//oapi:noreq
		{pattern: "POST /api/keyrings/deliveries/{id}/sync", cap: caps.KeyringsEdit, scope: scopeGlobal, h: s.handleSyncKeyringDelivery,
			resp: keyringDeliveryView{}, ts: "syncKeyringDelivery"},
		// The volume and its contents are left in place — deleting key material out from
		// under a running application is a worse surprise than a stale file, and there is
		// no re-issuing the data the app encrypted with it.
		//oapi:summary Delete a delivery, leaving the volume's files in place
		{pattern: "DELETE /api/keyrings/deliveries/{id}", cap: caps.KeyringsEdit, scope: scopeGlobal, h: s.handleDeleteKeyringDelivery,
			resp: map[string]string(nil), ts: "deleteKeyringDelivery"},

		// ── users ──────────────────────────────────────────────────────────────────
		//oapi:summary List users, with the roles each one holds
		//oapi:enum User.kind local|oidc
		//oapi:enum Membership.source local|oidc
		{pattern: "GET /api/users", cap: caps.UsersView, scope: scopeGlobal, h: s.handleListUsers,
			resp: []userView(nil), ts: "users"},
		// Creates a LOCAL user. OIDC users are not created here — they appear on first
		// successful sign-in, with the roles their claims map to.
		//oapi:summary Create a local user with at least one role
		//oapi:status 201
		//oapi:example req {"username": "jamila", "email": "jamila@example.com", "password": "a-long-placeholder-passphrase", "grants": [{"role_id": "role_operator", "env_id": ""}]}
		//oapi:required CreateUserRequest.username CreateUserRequest.password CreateUserRequest.grants
		{pattern: "POST /api/users", cap: caps.UsersEdit, scope: scopeGlobal, h: s.handleCreateUser,
			req: createUserRequest{}, resp: userView{}, ts: "createUser"},
		// Absent fields are left alone; disabling the last administrator is refused.
		//oapi:summary Update a user's email, or enable/disable the account
		{pattern: "PATCH /api/users/{id}", cap: caps.UsersEdit, scope: scopeGlobal, h: s.handleUpdateUser,
			req: updateUserRequest{}, resp: userView{}, ts: "updateUser"},
		//oapi:summary Delete a user; deleting the last administrator is refused
		{pattern: "DELETE /api/users/{id}", cap: caps.UsersEdit, scope: scopeGlobal, h: s.handleDeleteUser,
			resp: statusResponse{}, ts: "deleteUser"},
		// Local accounts only — an OIDC account's password lives at its provider. Token
		// callers are refused: a token that can change a password re-keys an account it
		// will outlive. No ts: the handwritten client wraps the bare string itself.
		//oapi:summary Set a local user's password
		//oapi:required PasswordRequest.password
		{pattern: "PUT /api/users/{id}/password", cap: caps.UsersEdit, scope: scopeGlobal, h: s.handleSetUserPassword,
			req: passwordRequest{}, resp: statusResponse{}},
		// Replaces the LOCALLY granted roles with exactly these grants; roles the identity
		// provider granted are untouched — they are re-synced on every login. No ts: the
		// handwritten client wraps the grants array itself.
		//oapi:summary Replace a user's locally granted roles
		{pattern: "PUT /api/users/{id}/roles", cap: caps.UsersEdit, scope: scopeGlobal, h: s.handleSetUserRoles,
			req: rolesRequest{}, resp: userView{}},

		// ── roles ──────────────────────────────────────────────────────────────────
		// roles.edit is administrative in the fullest sense: anyone holding it can grant
		// themselves every capability in any role they can touch.
		//oapi:summary List roles, with effective capabilities and member counts
		{pattern: "GET /api/roles", cap: caps.RolesView, scope: scopeGlobal, h: s.handleListRoles,
			resp: []roleView(nil), ts: "roles"},
		// Capabilities are addressed by NAME on the wire, never by a raw bitmask — names
		// can only ever mean what the server says they mean.
		//oapi:summary Create a role from capability names
		//oapi:status 201
		//oapi:example req {"name": "Operator", "description": "Deploy and restart, not administer", "cap_names": ["containers.view", "containers.edit", "stacks.view", "stacks.edit"]}
		//oapi:required RoleRequest.name
		{pattern: "POST /api/roles", cap: caps.RolesEdit, scope: scopeGlobal, h: s.handleCreateRole,
			req: roleRequest{}, resp: roleView{}, ts: "createRole"},
		//oapi:summary Rename a role or replace its capabilities
		{pattern: "PUT /api/roles/{id}", cap: caps.RolesEdit, scope: scopeGlobal, h: s.handleUpdateRole,
			req: roleRequest{}, resp: roleView{}, ts: "updateRole"},
		//oapi:summary Delete a role; the built-in Admin role and the last admin grant are refused
		{pattern: "DELETE /api/roles/{id}", cap: caps.RolesEdit, scope: scopeGlobal, h: s.handleDeleteRole,
			resp: statusResponse{}, ts: "deleteRole"},

		// ── resource monitors ──────────────────────────────────────────────────────
		//
		// The SAMPLES are containers.view at the host: a container's history is no more
		// sensitive than the live stats panel that capability already grants.
		//
		// The MONITORS are their own capability, and it is env-scopable — see the note in
		// caps.go for why they are not folded into settings.*.
		// Downsampled to at most ~240 points per series — a chart, not an export. The
		// client method stays handwritten (it builds the query string).
		//oapi:summary Read a CPU/memory time series for charts
		//oapi:query range string one of 1h, 6h, 24h, 7d (default 1h)
		//oapi:query container string narrow to one container name
		//oapi:query stack string narrow to one stack
		//oapi:query host boolean the machine's own CPU/memory instead of the container aggregate
		{pattern: "GET /api/clusters/{cluster}/metrics", cap: caps.ContainersView, scope: scopeEnv, h: s.handleSeries,
			resp: []store.Point(nil)},

		// A host-scoped holder sees the monitors pinned to their hosts — not the
		// fleet-wide ones, which watch hosts they have no standing on.
		//oapi:summary List every monitor the caller may view
		//oapi:enum Monitor.metric cpu_pct|mem_pct|mem_bytes|cpu_cores
		{pattern: "GET /api/monitors", cap: caps.MonitorsView, scope: scopeAny, h: s.handleListMonitors,
			resp: []*store.Monitor(nil), ts: "monitors"},
		// Firing first, then recent history; filtered to the hosts the caller may see.
		//oapi:summary List alerts, firing first
		//oapi:query limit integer at most this many alerts (default 100, max 500)
		{pattern: "GET /api/monitors/alerts", cap: caps.MonitorsView, scope: scopeAny, h: s.handleListAlerts,
			resp: []*store.Alert(nil), ts: "alerts"},
		// The host is in the BODY — and an absent one means "every host". The handler checks;
		// nothing else can. This is the third such route, and TestBodyScopedRoutesAreKnown
		// exists precisely so that adding it had to be a decision.
		//oapi:summary Create a resource monitor
		//oapi:status 201
		//oapi:example req {"name": "staging memory", "enabled": true, "metric": "mem_pct", "op": ">", "threshold": 80, "duration_secs": 600, "env_id": "env_staging"}
		{pattern: "POST /api/monitors", cap: caps.MonitorsEdit, scope: scopeBody, h: s.handleCreateMonitor,
			req: store.Monitor{}, resp: store.Monitor{}, ts: "createMonitor"},
		//oapi:summary Update a resource monitor
		{pattern: "PUT /api/monitors/{id}", cap: caps.MonitorsEdit, scope: scopeMonitor, h: s.handleUpdateMonitor,
			req: store.Monitor{}, resp: store.Monitor{}, ts: "updateMonitor"},
		// Its alert history goes with it.
		//oapi:summary Delete a resource monitor
		//oapi:status 204
		{pattern: "DELETE /api/monitors/{id}", cap: caps.MonitorsEdit, scope: scopeMonitor, h: s.handleDeleteMonitor,
			ts: "deleteMonitor"},

		// Sampling interval and retention are one setting for the whole fleet, so they take the
		// capability globally rather than on a host.
		//oapi:summary Read the sampling settings, limits and current disk usage
		{pattern: "GET /api/settings/monitoring", cap: caps.MonitorsView, scope: scopeGlobal, h: s.handleGetMonitorSettings,
			resp: monitorConfigResponse{}, ts: "monitorConfig"},
		//oapi:summary Set the sampling interval and retention
		//oapi:example req {"enabled": true, "interval_secs": 30, "retention_days": 7}
		{pattern: "PUT /api/settings/monitoring", cap: caps.MonitorsEdit, scope: scopeGlobal, h: s.handleSaveMonitorSettings,
			req: monitorSettingsRequest{}, resp: store.MonitorSettings{}, ts: "saveMonitorConfig"},

		// ── container log defaults ─────────────────────────────────────────────────
		// The fleet default is one setting, so it takes logging.* globally (the
		// monitoring-settings precedent); a host's override takes it at that host —
		// see the /api/clusters/{cluster}/logging trio.
		//oapi:summary Read the fleet-wide container log defaults
		{pattern: "GET /api/settings/logging", cap: caps.LoggingView, scope: scopeGlobal, h: s.handleGetGlobalLogConfig,
			resp: (*store.LogConfig)(nil), ts: "globalLogConfig"},
		// Injected into every deployed service that does not declare its own logging:
		// block. Rotation (max-size/max-file) IS the retention mechanism.
		//oapi:summary Set the fleet-wide container log defaults
		//oapi:example req {"driver": "json-file", "opts": {"max-size": "10m", "max-file": "3"}}
		{pattern: "PUT /api/settings/logging", cap: caps.LoggingEdit, scope: scopeGlobal, h: s.handleSaveGlobalLogConfig,
			req: logConfigRequest{}, resp: store.LogConfig{}, ts: "saveGlobalLogConfig"},
		// Unset means "inject nothing" — the daemon's own default applies. Idempotent.
		//oapi:summary Unset the fleet-wide container log defaults
		//oapi:status 204
		{pattern: "DELETE /api/settings/logging", cap: caps.LoggingEdit, scope: scopeGlobal, h: s.handleDeleteGlobalLogConfig,
			ts: "clearGlobalLogConfig"},

		// ── identity providers ─────────────────────────────────────────────────────
		// Responses carry has_secret, never the client secret: it is sealed with the
		// master key and there is no endpoint that reads it back.
		//oapi:summary List identity providers
		{pattern: "GET /api/settings/oidc", cap: caps.SettingsView, scope: scopeGlobal, h: s.handleListProviders,
			resp: []providerView(nil), ts: "providers"},
		//oapi:summary Add an identity provider
		//oapi:status 201
		//oapi:required ProviderRequest.slug ProviderRequest.name ProviderRequest.issuer ProviderRequest.client_id ProviderRequest.redirect_url
		{pattern: "POST /api/settings/oidc", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleCreateProvider,
			req: providerRequest{}, resp: providerView{}, ts: "createProvider"},
		// An empty client_secret means "keep the stored one", so an edit form that does
		// not resend it cannot blank it by omission.
		//oapi:summary Update an identity provider
		{pattern: "PUT /api/settings/oidc/{id}", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleUpdateProvider,
			req: providerRequest{}, resp: providerView{}, ts: "updateProvider"},
		//oapi:summary Delete an identity provider
		{pattern: "DELETE /api/settings/oidc/{id}", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleDeleteProvider,
			resp: statusResponse{}, ts: "deleteProvider"},
		// Fetches the discovery document: the cheap version of "does this actually
		// work", answered on the settings page instead of at somebody's failed login.
		//oapi:summary Test a provider by fetching its discovery document
		//oapi:noreq
		{pattern: "POST /api/settings/oidc/{id}/test", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleTestProvider,
			resp: testResult{}, ts: "testProvider"},
		//oapi:summary List a provider's claim-to-role mappings
		//oapi:enum Scope.Kind global|env
		{pattern: "GET /api/settings/oidc/{id}/mappings", cap: caps.SettingsView, scope: scopeGlobal, h: s.handleListMappings,
			resp: []store.OIDCRoleMapping(nil), ts: "mappings"},
		// Takes effect at each user's next sign-in, when their provider roles re-sync.
		//oapi:summary Map a claim value to a role, everywhere or on one host
		//oapi:status 201
		//oapi:required MappingRequest.claim_value MappingRequest.role_id
		{pattern: "POST /api/settings/oidc/{id}/mappings", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleCreateMapping,
			req: mappingRequest{}, resp: store.OIDCRoleMapping{}, ts: "createMapping"},
		//oapi:summary Delete a claim-to-role mapping
		{pattern: "DELETE /api/settings/oidc/{id}/mappings/{mapping}", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleDeleteMapping,
			resp: statusResponse{}, ts: "deleteMapping"},

		// ── notifications ──────────────────────────────────────────────────────────
		// Same capability as the identity providers: this is Settings, and the SMTP
		// password is a secret of exactly the same kind as an OIDC client secret.
		// Responses carry has_password, never the password: it is sealed with the
		// master key and there is no endpoint that reads it back.
		//oapi:summary Read the SMTP settings
		{pattern: "GET /api/settings/smtp", cap: caps.SettingsView, scope: scopeGlobal, h: s.handleGetSMTP,
			resp: smtpView{}, ts: "smtp"},
		// An empty password means "keep the stored one" — the OIDC client secret rule.
		//oapi:summary Save the SMTP settings
		//oapi:example req {"host": "smtp.example.com", "port": 587, "username": "daffa", "password": "placeholder", "from_addr": "daffa@example.com", "from_name": "Daffa", "base_url": "https://daffa.example.com", "enabled": true}
		{pattern: "PUT /api/settings/smtp", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleSaveSMTP,
			req: smtpRequest{}, resp: smtpView{}, ts: "saveSmtp"},
		// Sends one real email to the caller's own address, synchronously, bypassing the
		// outbox — a queued test that fails silently later answers nothing. ok says
		// whether it worked; error carries the SMTP server's own words.
		//oapi:summary Send a test email to your own address
		//oapi:noreq
		{pattern: "POST /api/settings/smtp/test", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleTestSMTP,
			resp: testResult{}, ts: "testSmtp"},
		//oapi:summary List every event a notification rule can route
		{pattern: "GET /api/notifications/events", cap: caps.SettingsView, scope: scopeGlobal, h: s.handleListNotifyEvents,
			resp: []notifyEventView(nil), ts: "notifyEvents"},
		// Renders the event with plausible data — the only way to see what one looks
		// like without breaking something first.
		//oapi:summary Preview an event's rendered notification
		//oapi:path event the event's wire name, e.g. deploy.failed
		{pattern: "GET /api/notifications/preview/{event}", cap: caps.SettingsView, scope: scopeGlobal, h: s.handlePreviewNotification,
			resp: notifyPreviewResponse{}, ts: "previewNotification"},
		// The dead-letter list: an alert that failed to send and then vanished would
		// leave you believing nothing went wrong.
		//oapi:summary List recently failed notifications
		{pattern: "GET /api/notifications/failed", cap: caps.SettingsView, scope: scopeGlobal, h: s.handleFailedNotifications,
			resp: []failedNotificationView(nil), ts: "failedNotifications"},
		//oapi:summary List the notification routing rules
		{pattern: "GET /api/notifications/rules", cap: caps.SettingsView, scope: scopeGlobal, h: s.handleListNotifyRules,
			resp: []store.NotificationRule(nil), ts: "notifyRules"},
		// A rule routes one event to exactly one target: a role, an email address, or a
		// channel.
		//oapi:summary Route an event to a role, an address, or a channel
		//oapi:status 201
		//oapi:example req {"event": "deploy.failed", "role_id": "", "address": "", "channel_id": "ch_placeholder"}
		//oapi:required RuleRequest.event
		{pattern: "POST /api/notifications/rules", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleCreateNotifyRule,
			req: ruleRequest{}, resp: store.NotificationRule{}, ts: "createNotifyRule"},
		//oapi:summary Delete a notification routing rule
		{pattern: "DELETE /api/notifications/rules/{id}", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleDeleteNotifyRule,
			resp: statusResponse{}, ts: "deleteNotifyRule"},
		// Chat/webhook channels. A channel URL is a bearer credential, so editing them is
		// SettingsEdit and the URL is sealed and never read back — the same treatment as the SMTP
		// password two blocks up.
		//oapi:summary List notification channels
		//oapi:enum NotifyChannel.kind slack|discord|webhook
		{pattern: "GET /api/notifications/channels", cap: caps.SettingsView, scope: scopeGlobal, h: s.handleListChannels,
			resp: []channelView(nil), ts: "notifyChannels"},
		// The server POSTs a real "connected" message before saving — a webhook that
		// 404s is not a configuration, it is a future silent failure.
		//oapi:summary Add a channel, proving the webhook answers before saving it
		//oapi:status 201
		//oapi:required ChannelRequest.kind ChannelRequest.name ChannelRequest.url
		{pattern: "POST /api/notifications/channels", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleCreateChannel,
			req: channelRequest{}, resp: channelView{}, ts: "createNotifyChannel"},
		//oapi:summary Re-post the connected message to a saved channel
		//oapi:noreq
		{pattern: "POST /api/notifications/channels/{id}/test", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleTestChannel,
			resp: testResult{}, ts: "testNotifyChannel"},
		//oapi:summary Delete a channel; its routing rules cascade away
		{pattern: "DELETE /api/notifications/channels/{id}", cap: caps.SettingsEdit, scope: scopeGlobal, h: s.handleDeleteChannel,
			resp: statusResponse{}, ts: "deleteNotifyChannel"},

		// ── audit ──────────────────────────────────────────────────────────────────
		// Its own capability. Who tried what, and was refused, is exactly the sort of thing
		// you do not hand to everyone with a login by default. Entries with no host —
		// users, roles, identity providers — are shown only to a GLOBAL audit.view holder.
		//oapi:summary List audit entries, newest first
		//oapi:query limit integer at most this many entries (default 200, max 500)
		//oapi:enum AuditEntry.outcome ok|error|denied
		{pattern: "GET /api/audit", cap: caps.AuditView, scope: scopeAny, h: s.handleAudit,
			resp: []auditEntryView(nil), ts: "audit"},
	}
}

// Handler builds the full route tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ── unauthenticated ────────────────────────────────────────────────────────
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Agent endpoints: machine-authenticated (bearer), deliberately outside /api/ so a
	// browser session cookie can never reach them.
	mux.HandleFunc("POST /agents/enroll", s.handleAgentEnroll)
	mux.HandleFunc("GET /agents/connect", s.handleAgentConnect)

	// The git webhook: called by a machine on the other side of a firewall, authenticated
	// by a per-stack HMAC over the body. Also outside /api/ — a session cookie must never
	// be able to reach a route that starts a deploy without one.
	mux.HandleFunc("POST /webhooks/stacks/{id}", s.handleWebhook)
	// The volume-source twin: same posture, same HMAC authentication, same "no session
	// can reach it and no cookie helps" reasoning.
	mux.HandleFunc("POST /webhooks/volume-sources/{id}", s.handleVolumeSourceWebhook)

	// What the login page needs to render itself: which methods are enabled, and one
	// button per identity provider.
	mux.HandleFunc("GET /api/auth/config", s.handleAuthConfig)
	mux.HandleFunc("POST /api/auth/login", s.handleLocalLogin)
	mux.HandleFunc("GET /api/auth/oidc/start/{provider}", s.handleOIDCStart)
	mux.HandleFunc("GET /api/auth/callback/{provider}", s.handleOIDCCallback)
	mux.HandleFunc("GET /api/auth/break-glass", s.handleBreakGlass)

	// ── authenticated ──────────────────────────────────────────────────────────
	// One registration per route, with its capability beside it. The previous shape —
	// each privileged route registered twice, once on a role sub-mux and once wrapped on
	// the main one — had no way to notice when the two disagreed.
	api := http.NewServeMux()
	for _, rt := range s.apiRoutes() {
		api.Handle(rt.pattern, s.guard(rt))
	}

	mux.Handle("/api/", s.sessions.Require(api))

	// ── SPA ────────────────────────────────────────────────────────────────────
	mux.Handle("/", web.Handler())

	// CSRF wraps everything: it is a property of the request, not of a route. logging is
	// OUTERMOST so its one line per request captures everything inside it — a CSRF rejection, a
	// panic in any middleware — not just what reached a handler.
	return logging(auth.CSRF("")(noIndex(mux)))
}

// noIndex tells every crawler, on every response, that there is nothing here for it.
//
// Daffa is a private operations console. It knows your container names, your image tags, your
// repository URLs and an audit log of who did what — and a self-hosted install very often sits
// on a real hostname, reachable from the internet, with nobody having thought about robots.
//
// This is a HEADER and not only a <meta> tag, because a meta tag is seen only by something that
// parses the HTML document containing it. That covers the SPA shell and nothing else: not an API
// response, not an asset, and not a URL that never renders. And because the SPA hands back
// index.html with a 200 for ANY unknown path, a crawler that wanders in finds an endless supply
// of "pages" that all look different to it and identical to us.
//
// X-Robots-Tag covers every response the server makes, which is the only coverage worth having.
// It is belt and braces with robots.txt: robots.txt asks a crawler not to FETCH, and is a public
// file that itself advertises what exists; X-Robots-Tag tells it not to INDEX what it fetched
// anyway. Well-behaved crawlers honour both, and neither is a security control — the session
// cookie is. This is about not showing up in a search result.
func noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive, nosnippet, noimageindex")
		next.ServeHTTP(w, r)
	})
}

// guard wraps a route in its capability check, at the scope the route declares. An open
// route is passed through unchanged — but only after the table has forced someone to write
// down why.
func (s *Server) guard(rt route) http.Handler {
	switch rt.scope {
	case scopeNone:
		// Open. handleExec still checks containers.exec itself — see exec_handler.go.
		return rt.h

	case scopeGlobal:
		// nil extractor ⇒ only a global grant satisfies it.
		return auth.RequireCap(rt.cap, nil, s.recordDenial)(rt.h)

	case scopeEnv:
		return auth.RequireCap(rt.cap, envFromPath, s.recordDenial)(rt.h)

	case scopeAny:
		return auth.RequireCapAnywhere(rt.cap, s.recordDenial)(rt.h)

	case scopeStack:
		return s.requireOnStack(rt.cap)(rt.h)

	case scopeDeployment:
		return s.requireOnDeployment(rt.cap)(rt.h)

	case scopeJob:
		return s.requireOnJob(rt.cap)(rt.h)

	case scopeMonitor:
		return s.requireOnMonitor(rt.cap)(rt.h)

	case scopeVolumeSource:
		return s.requireOnVolumeSource(rt.cap)(rt.h)

	case scopeBody:
		// The environment is in the request body, which no middleware can read. The
		// handler checks it after decoding, via s.mayUseEnv. Passing through here is a
		// deliberate hole, and TestBodyScopedRoutesAreKnown pins the list so a third one
		// cannot appear without somebody choosing to add it.
		return rt.h

	default:
		// scopeUnset. Unreachable — TestEveryRouteIsGuarded fails first — but a panic here
		// beats serving an unguarded route if the test is ever weakened.
		panic("api: route " + rt.pattern + " declares no scope")
	}
}

func envFromPath(r *http.Request) string { return r.PathValue("cluster") }

// visible answers "which hosts may this person exercise c on, and is it fleet-wide?" — the
// two arguments every filtered list query needs.
func visible(r *http.Request, c caps.Cap) (global bool, envs []string) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		return false, nil
	}
	if u.Caps.Global.Has(c) {
		return true, nil
	}
	for _, id := range u.Caps.Envs() {
		if u.Caps.Env[id].Has(c) {
			envs = append(envs, id)
		}
	}
	return false, envs
}

// requireOnStack resolves the {id} path value to a stack, checks the capability at THAT
// stack's environment, and stashes the stack so the handler's s.stack() does not pay for
// the lookup again.
//
// The resolution happens here, in the middleware, rather than being left to the handler.
// A handler that forgot to call the chokepoint would otherwise be silently unguarded — and
// "we always remember to call the helper" is not a property a codebase keeps for long.
func (s *Server) requireOnStack(c caps.Cap) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := auth.UserFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}

			stack, err := s.store.StackByID(r.Context(), r.PathValue("id"))
			if errors.Is(err, store.ErrNotFound) {
				httpx.Fail(w, r, http.StatusNotFound, "no_such_stack", "No such stack.")
				return
			}
			if err != nil {
				httpx.Error(w, r, err)
				return
			}

			// A stack on a host you hold nothing on is a stack you cannot see. 404, not
			// 403: telling someone "that exists, but not for you" is itself information.
			if !u.Caps.Has(c, stack.EnvID) {
				s.recordDenial(r, u, "missing_capability:"+c.Name())
				httpx.Fail(w, r, http.StatusNotFound, "no_such_stack", "No such stack.")
				return
			}

			next.ServeHTTP(w, r.WithContext(withStack(r.Context(), stack)))
		})
	}
}

// requireOnDeployment resolves the {id} path value to a deployment, then to its stack, and
// checks the capability at THAT stack's host.
//
// A deployment does not carry a host of its own — its stack does. So the check is one hop
// longer, and it is exactly the hop that would have been forgotten if this were left to the
// handlers: a deployment id is a global handle, and serving one without walking back to the
// stack that owns it would hand every deploy log in the fleet to anyone who could guess an id.
func (s *Server) requireOnDeployment(c caps.Cap) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := auth.UserFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}

			dep, err := s.store.DeploymentByID(r.Context(), r.PathValue("id"))
			if errors.Is(err, store.ErrNotFound) {
				httpx.Fail(w, r, http.StatusNotFound, "no_such_deployment", "No such deployment.")
				return
			}
			if err != nil {
				httpx.Error(w, r, err)
				return
			}

			stack, err := s.store.StackByID(r.Context(), dep.StackID)
			if errors.Is(err, store.ErrNotFound) {
				httpx.Fail(w, r, http.StatusNotFound, "no_such_deployment", "No such deployment.")
				return
			}
			if err != nil {
				httpx.Error(w, r, err)
				return
			}

			// 404, not 403, for the same reason as a stack: "that exists, but not for you" is
			// itself information.
			if !u.Caps.Has(c, stack.EnvID) {
				s.recordDenial(r, u, "missing_capability:"+c.Name())
				httpx.Fail(w, r, http.StatusNotFound, "no_such_deployment", "No such deployment.")
				return
			}

			next.ServeHTTP(w, r.WithContext(withDeployment(r.Context(), dep, stack)))
		})
	}
}

// requireOnJob does the same for a backup job.
func (s *Server) requireOnJob(c caps.Cap) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := auth.UserFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}

			job, err := s.store.BackupJobByID(r.Context(), r.PathValue("id"))
			if errors.Is(err, store.ErrNotFound) {
				httpx.Fail(w, r, http.StatusNotFound, "no_such_job", "No such backup job.")
				return
			}
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			if !u.Caps.Has(c, job.EnvID) {
				s.recordDenial(r, u, "missing_capability:"+c.Name())
				httpx.Fail(w, r, http.StatusNotFound, "no_such_job", "No such backup job.")
				return
			}

			next.ServeHTTP(w, r.WithContext(withJob(r.Context(), job)))
		})
	}
}

// requireOnMonitor resolves a monitor and checks the caller may act on it.
//
// A monitor watching ONE host is checked at that host, like a stack or a backup job. A monitor
// watching NO PARTICULAR host watches every host — it is a fleet-wide rule — and so it takes
// the capability globally. A host-scoped holder who could edit it could point it at production
// and start receiving production container names by mail.
//
// 404 rather than 403 on a monitor they cannot see, for the same reason a stack does: "that
// exists, but not for you" is itself information.
func (s *Server) requireOnMonitor(c caps.Cap) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := auth.UserFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}

			m, err := s.store.MonitorByID(r.Context(), r.PathValue("id"))
			if errors.Is(err, store.ErrNotFound) {
				httpx.Fail(w, r, http.StatusNotFound, "no_such_monitor", "No such monitor.")
				return
			}
			if err != nil {
				httpx.Error(w, r, err)
				return
			}

			allowed := u.Caps.Has(c, m.EnvID)
			if m.EnvID == "" {
				// Fleet-wide: only a global grant will do.
				allowed = u.Caps.Global.Has(c)
			}
			if !allowed {
				s.recordDenial(r, u, "missing_capability:"+c.Name())
				httpx.Fail(w, r, http.StatusNotFound, "no_such_monitor", "No such monitor.")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// requireOnVolumeSource resolves a volume source and checks the capability at the host it
// delivers to. 404 rather than 403 on a source they cannot see, the stack precedent:
// "that exists, but not for you" is itself information.
func (s *Server) requireOnVolumeSource(c caps.Cap) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := auth.UserFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}

			v, err := s.store.VolumeSourceByID(r.Context(), r.PathValue("id"))
			if errors.Is(err, store.ErrNotFound) {
				httpx.Fail(w, r, http.StatusNotFound, "no_such_source", "No such volume source.")
				return
			}
			if err != nil {
				httpx.Error(w, r, err)
				return
			}

			if !u.Caps.Has(c, v.EnvID) {
				s.recordDenial(r, u, "missing_capability:"+c.Name())
				httpx.Fail(w, r, http.StatusNotFound, "no_such_source", "No such volume source.")
				return
			}

			next.ServeHTTP(w, r.WithContext(withVolumeSource(r.Context(), v)))
		})
	}
}

// mayUseEnv is the check the body-scoped creates make for themselves, once they have
// decoded the environment they were asked to act on. It is the one authorization decision
// that a route table cannot make.
func (s *Server) mayUseEnv(w http.ResponseWriter, r *http.Request, c caps.Cap, envID string) bool {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return false
	}
	if !u.Caps.Has(c, envID) {
		auth.Deny(w, r, u, c, s.recordDenial)
		return false
	}
	return true
}

// The resolved target travels in the request context, so the middleware's lookup is not
// repeated by the handler.
type ctxKey int

const (
	stackKey ctxKey = iota
	jobKey
	deploymentKey
	volSourceKey
)

func withStack(ctx context.Context, st *store.Stack) context.Context {
	return context.WithValue(ctx, stackKey, st)
}
func stackFrom(ctx context.Context) (*store.Stack, bool) {
	st, ok := ctx.Value(stackKey).(*store.Stack)
	return st, ok
}

// A deployment always travels with its stack: every handler that has one needs the other (for
// the host to reach, the name to show, the project to act on), and resolving it twice would be
// a second query for something the guard already had in hand.
type deploymentCtx struct {
	dep   *store.Deployment
	stack *store.Stack
}

func withDeployment(ctx context.Context, d *store.Deployment, st *store.Stack) context.Context {
	return context.WithValue(ctx, deploymentKey, deploymentCtx{dep: d, stack: st})
}
func deploymentFrom(ctx context.Context) (*store.Deployment, *store.Stack, bool) {
	v, ok := ctx.Value(deploymentKey).(deploymentCtx)
	if !ok {
		return nil, nil, false
	}
	return v.dep, v.stack, true
}
func withJob(ctx context.Context, j *store.BackupJob) context.Context {
	return context.WithValue(ctx, jobKey, j)
}
func jobFrom(ctx context.Context) (*store.BackupJob, bool) {
	j, ok := ctx.Value(jobKey).(*store.BackupJob)
	return j, ok
}
func withVolumeSource(ctx context.Context, v *store.VolumeSource) context.Context {
	return context.WithValue(ctx, volSourceKey, v)
}
func volSourceFrom(ctx context.Context) (*store.VolumeSource, bool) {
	v, ok := ctx.Value(volSourceKey).(*store.VolumeSource)
	return v, ok
}

// logging emits one structured line per request. Streams (SSE) log at start, since
// their duration is the user's dwell time, not a latency number worth alerting on.
// logging writes exactly ONE structured line per request, and it is where the request's whole
// story lands: the method, path, status and duration always; the failure reason (code, message,
// and for a 500 the underlying error) when there was one, put there by httpx via the recorder;
// and a panic's stack, because an unrecovered panic would otherwise be logged only by net/http's
// default as an unstructured line with no request context — the single hardest thing to debug,
// logged the least. Mounted OUTERMOST so it sees everything, CSRF rejections and all.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Deferred so the line is written even when the handler panics — and so is the 500 the
		// caller is owed, if nothing has been sent yet.
		defer func() {
			if p := recover(); p != nil {
				slog.Error("panic serving request",
					"method", r.Method, "path", r.URL.Path, "panic", p,
					"stack", string(debug.Stack()))
				if !rec.wrote && !rec.hijacked {
					httpx.Error(rec, r, fmt.Errorf("panic: %v", p))
				} else {
					rec.status = http.StatusInternalServerError
				}
			}

			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			} else if rec.status >= 400 {
				level = slog.LevelWarn
			}

			attrs := []any{
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"ms", time.Since(start).Milliseconds(),
			}
			// The reason a request failed, from httpx via RecordError. `error` is what the caller
			// was told; `cause` is the raw server-side error a 500 hid from them.
			if rec.code != "" {
				attrs = append(attrs, "error_code", rec.code)
				if rec.message != "" {
					attrs = append(attrs, "error", rec.message)
				}
			}
			if rec.err != nil {
				attrs = append(attrs, "cause", rec.err.Error())
			}
			slog.Log(r.Context(), level, "http", attrs...)
		}()

		next.ServeHTTP(rec, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status   int
	code     string // the failure code the caller was given, if any
	message  string // the failure message the caller was given
	err      error  // the underlying server-side error a 500 hid, for the log only
	wrote    bool   // a status or body has gone out; a later 500 can no longer be written
	hijacked bool   // the connection was taken over (WebSocket, exec) — do not touch it
}

// RecordError implements httpx.ErrorRecorder: httpx.Fail/Error hand the failure reason here so
// the access-log line can carry it, instead of each failure logging a separate line of its own.
func (r *statusRecorder) RecordError(code, message string, err error) {
	r.code, r.message, r.err = code, message, err
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Wrapping a ResponseWriter hides the optional interfaces the real one implements, and
// the streaming parts of Daffa depend on exactly those. Flush must pass through or SSE
// buffers forever; Hijack must pass through or the WebSocket upgrade fails with 501 and
// the exec terminal never opens at all.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("api: the underlying ResponseWriter does not support hijacking")
	}
	r.hijacked = true
	return h.Hijack()
}

// recordDenial audits an action the capability check refused. What someone TRIED to do is
// as much a part of the record as what they managed to do.
//
// The action name is derived from the route rather than passed in, so a new privileged
// route cannot be added without its denials being labelled — the failure mode of the
// alternative is an audit line that says "request" and tells you nothing.
func (s *Server) recordDenial(r *http.Request, u *store.User, reason string) {
	target := firstNonEmpty(r.PathValue("id"), r.PathValue("name"))

	var action string
	switch {
	case r.PathValue("action") != "":
		action = "container." + r.PathValue("action")
	case r.PathValue("target") != "":
		action = "prune." + r.PathValue("target")
	case strings.HasSuffix(r.URL.Path, "/exec"):
		action = "container.exec"
	case r.Method == http.MethodDelete:
		// /api/clusters/{cluster}/images/{id} → images
		action = resourceFromPath(r.URL.Path) + ".remove"
	default:
		action = r.Method + " " + r.URL.Path
	}

	// The reason carries the missing capability's name (missing_capability:containers.exec),
	// so the log says what the person lacked rather than merely that they lacked something.
	// held is what they DID have — the pair is what makes a denial diagnosable without
	// reconstructing the role table as it was at the time.
	s.audit(r.Context(), store.AuditEntry{
		UserID: u.ID, UserLabel: u.Label(),
		EnvID:   r.PathValue("cluster"),
		Action:  action,
		Target:  target,
		Outcome: "denied",
		Detail: store.AuditDetail(map[string]any{
			"reason": reason,
			"held":   u.Caps.Global.Names(),
			"ip":     s.clientIP(r),
		}),
	})
}

// resourceFromPath pulls the collection out of /api/clusters/{cluster}/<collection>/…
func resourceFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// api / environments / {cluster} / <collection> / …
	if len(parts) >= 4 {
		return strings.TrimSuffix(parts[3], "s")
	}
	return "resource"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// audit records a mutating action. It never fails the operation it is describing —
// a lost audit line is bad, but refusing to restart a container because the audit
// insert failed is worse, and the error still reaches the log.
func (s *Server) audit(ctx context.Context, e store.AuditEntry) {
	if u, ok := auth.UserFrom(ctx); ok {
		e.UserID, e.UserLabel = u.ID, u.Label()
		// A token-authenticated mutation names its credential: "which token did this"
		// is the question a leaked token makes urgent, and the action column alone
		// cannot answer it.
		if t, ok := auth.TokenFrom(ctx); ok {
			e.UserLabel += " (token: " + t.Name + ")"
		}
	}
	if err := s.store.Audit(ctx, e); err != nil {
		slog.Error("writing audit entry", "action", e.Action, "err", err)
	}
}
