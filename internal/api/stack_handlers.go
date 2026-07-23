package api

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/certs"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// ── stacks ──────────────────────────────────────────────────────────────────────

// statusResponse is the "it happened" answer mutations give when there is no entity to
// return — the wire shape is {"status": "ok"} (or another one-word outcome).
type stackView struct {
	ID    string `json:"id"`
	EnvID string `json:"env_id"`
	Name  string `json:"name"`

	// Engine is the stored value; EngineLabel is what a person reads. Actions is what this
	// engine can actually do.
	//
	// The UI renders its buttons from Actions rather than from a list of its own. That is the
	// whole reason it is here: the engines do not agree on the verbs (swarm has no pull and no
	// stop), and a frontend that hardcoded compose's five would ship dead buttons the day a
	// swarm stack appeared.
	Engine      string   `json:"engine"`
	EngineLabel string   `json:"engine_label"`
	Actions     []string `json:"actions"`

	GroupName       string     `json:"group_name,omitempty"`
	SourceKind      string     `json:"source_kind"`
	GitURL          string     `json:"git_url,omitempty"`
	GitRef          string     `json:"git_ref,omitempty"`
	GitPath         string     `json:"git_path,omitempty"`
	GitCredentialID string     `json:"git_credential_id,omitempty"`
	InlineYAML      string     `json:"inline_yaml,omitempty"`
	DeployedAt      *time.Time `json:"deployed_at,omitempty"`
	// DeployedCommit is what is actually live. A hash says "the source moved"; only this says
	// which commit is running, which is the question people ask.
	DeployedCommit string `json:"deployed_commit,omitempty"`
	// LastDeployStatus lets the UI tell "nobody has ever deployed this" apart from "somebody
	// tried and it failed". Saying the first when you mean the second is how a stack that is
	// up and serving ends up labelled "never deployed".
	LastDeployStatus string `json:"last_deploy_status,omitempty"`

	AutoDeploy bool     `json:"auto_deploy"`
	WatchPaths string   `json:"watch_paths,omitempty"`
	Watching   []string `json:"watching,omitempty"` // resolved, including the default
	// HasSecret says a webhook is configured. The secret itself is shown exactly once,
	// when it is minted, and never handed back.
	HasSecret bool `json:"has_secret"`
}

func viewStack(s *store.Stack) stackView {
	v := stackView{
		ID: s.ID, EnvID: s.EnvID, Name: s.Name, GroupName: s.GroupName,
		SourceKind: s.SourceKind,
		GitURL:     s.GitURL, GitRef: s.GitRef, GitPath: s.GitPath,
		GitCredentialID:  s.GitCredentialID, // the credential's id; never its secret
		InlineYAML:       s.InlineYAML,
		AutoDeploy:       s.AutoDeploy,
		WatchPaths:       s.WatchPaths,
		HasSecret:        s.WebhookSecretEnc != "",
		DeployedCommit:   s.DeployedCommit,
		LastDeployStatus: s.LastDeployStatus,
		Actions:          []string{},
	}

	// An unknown engine is a stack that cannot be acted on, not a request that fails: the page
	// must still open, so somebody can see what it says and change it back.
	if eng, err := stacks.EngineFor(s.Engine); err == nil {
		v.Engine, v.EngineLabel = eng.Name(), eng.Label()
		for _, a := range eng.Actions() {
			v.Actions = append(v.Actions, string(a))
		}
	} else {
		v.Engine, v.EngineLabel = s.Engine, s.Engine
	}

	if s.SourceKind == "git" {
		v.Watching = stacks.WatchPatterns(s.WatchPaths, s.GitPath)
	}
	if !s.DeployedAt.IsZero() {
		t := s.DeployedAt
		v.DeployedAt = &t
	}
	return v
}

func (s *Server) handleListStacks(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.StacksView)
	list, err := s.store.ListStacks(r.Context(), global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]stackView, 0, len(list))
	for _, st := range list {
		out = append(out, viewStack(st))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type stackRequest struct {
	EnvID string `json:"env_id"`
	// NodeID is PLACEMENT: which machine the containers land on. Empty means "the environment
	// decides" — see placementFor.
	NodeID          string `json:"node_id"`
	Name            string `json:"name"`
	Engine          string `json:"engine"` // empty means compose
	GroupName       string `json:"group_name"`
	SourceKind      string `json:"source_kind"`
	GitURL          string `json:"git_url"`
	GitRef          string `json:"git_ref"`
	GitPath         string `json:"git_path"`
	GitCredentialID string `json:"git_credential_id"`
	InlineYAML      string `json:"inline_yaml"`
}

// engineFrom validates a requested engine, defaulting to compose.
//
// It refuses an engine Daffa cannot run rather than storing it and failing at deploy time. A
// stack that accepts an engine and then quietly runs a different one would be the exact confusion
// this change exists to end — only worse, because now the UI would be asserting it.
func (s *Server) engineFrom(w http.ResponseWriter, r *http.Request, name string) (stacks.Engine, bool) {
	eng, err := stacks.EngineFor(strings.TrimSpace(name))
	if err != nil {
		httpx.BadRequest(w, r, err.Error())
		return nil, false
	}
	return eng, true
}

// placementFor settles the two questions a stack must answer before it can be deployed, and they
// are DIFFERENT questions: how is the file applied (the engine), and where does it run (the node).
//
//	environment  engine   node_id     who picks the node
//	standalone   compose  implicit    there is only one
//	swarm        swarm    empty       THE SCHEDULER — which is the entire point of Swarm
//	swarm        compose  required*   YOU
//
// (*required only when the swarm has more than one node — the same arity rule as ?node=. A
// single-node swarm has exactly one answer, and demanding it would be pedantry aimed at the
// topology most people actually run.)
//
// That table IS the difference between compose and swarm on a cluster: who picks the node. Making
// somebody name it is not a burden, it is the truth of what they asked for — and it removes the
// hazard that would otherwise sink compose-on-a-swarm entirely. A pinned stack's runner always goes
// to the same daemon, its containers always land there, and a change of Swarm leadership is
// irrelevant to it. Resolve the runner from the control node instead, and a leadership change
// silently puts the next deploy on a different machine from the containers it is updating.
func (s *Server) placementFor(w http.ResponseWriter, r *http.Request, req *stackRequest, eng stacks.Engine) (string, bool) {
	env, err := s.pool.Get(req.EnvID)
	if err != nil {
		httpx.BadRequest(w, r, "That environment is not connected.")
		return "", false
	}

	swarmStack := eng.Name() == stacks.SwarmEngine.Name()

	// A swarm stack needs a Swarm. Refused here, and again at deploy time, because an environment's
	// kind can change between the two — somebody can run `docker swarm leave` in the meantime.
	if swarmStack && !env.IsSwarm() {
		httpx.BadRequest(w, r,
			"This environment is a standalone host, not a Swarm cluster, so it cannot run a Swarm stack.")
		return "", false
	}

	// A swarm stack is placed by the SCHEDULER. Pinning it to a node would be asking Swarm to be
	// Compose, and the answer to that is to use Compose.
	if swarmStack {
		if req.NodeID != "" {
			httpx.BadRequest(w, r,
				"A Swarm stack is placed by the scheduler, so it cannot be pinned to a node. "+
					"If you want to choose the machine yourself, deploy it with Compose instead.")
			return "", false
		}
		return "", true
	}

	// A compose stack on a swarm must say which machine. On a standalone environment, and on a
	// single-node swarm, there is exactly one answer and we fill it in.
	if req.NodeID != "" {
		if _, err := env.Node(req.NodeID); err != nil {
			httpx.BadRequest(w, r, "That environment has no such node.")
			return "", false
		}
		return req.NodeID, true
	}

	node, err := env.One()
	if err != nil {
		httpx.BadRequest(w, r,
			"This environment has more than one node, so a Compose stack has to say which one it "+
				"runs on. Its containers land on that machine, and only that machine.")
		return "", false
	}
	// Pin it explicitly even though it was implicit. A node that is obvious today stops being
	// obvious the moment a second one is enrolled, and a stack that silently changed machines
	// because the environment grew is not a thing anybody should have to debug.
	return node.ID, true
}

// validName keeps a compose project name to what compose itself accepts, rather than
// letting a bad name fail deep inside the runner where the error is unrecognizable.
func validProjectName(n string) bool {
	if n == "" || len(n) > 63 {
		return false
	}
	for i, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleCreateStack(w http.ResponseWriter, r *http.Request) {
	var req stackRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	if !validProjectName(req.Name) {
		httpx.BadRequest(w, r, "The name must be lowercase letters, digits, - or _ (it becomes the compose project name).")
		return
	}
	if req.SourceKind != "git" && req.SourceKind != "inline" {
		httpx.BadRequest(w, r, "The source must be git or inline.")
		return
	}

	eng, ok := s.engineFrom(w, r, req.Engine)
	if !ok {
		return
	}

	// The host arrives in the BODY, so no middleware could have checked it — this is the
	// one authorization decision the route table cannot make, and skipping it would let
	// anyone with stacks.edit on ANY host deploy to EVERY host.
	if !s.mayUseEnv(w, r, caps.StacksEdit, req.EnvID) {
		return
	}

	nodeID, ok := s.placementFor(w, r, &req, eng)
	if !ok {
		return
	}

	stack := &store.Stack{
		EnvID: req.EnvID, NodeID: nodeID, Name: req.Name, Engine: eng.Name(),
		GroupName:  strings.TrimSpace(req.GroupName),
		SourceKind: req.SourceKind,
		GitURL:     req.GitURL, GitRef: req.GitRef, GitPath: req.GitPath,
		GitCredentialID: req.GitCredentialID,
		InlineYAML:      req.InlineYAML,
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		stack.CreatedBy = u.ID
	}

	if err := s.store.CreateStack(r.Context(), stack); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken",
			"A stack with that name already exists on this environment.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: stack.EnvID, Action: "stack.create", Target: stack.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, viewStack(stack))
}

func (s *Server) handleUpdateStack(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}

	var req stackRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if req.SourceKind != "git" && req.SourceKind != "inline" {
		httpx.BadRequest(w, r, "The source must be git or inline.")
		return
	}

	// THE ENGINE IS IMMUTABLE. Changing it would orphan every container the old engine made, under
	// a label the new engine never looks at: `docker compose down` cannot see a swarm stack's
	// services, and `docker stack rm` cannot see a compose stack's containers. The old workload
	// would keep running, invisible to Daffa, with nothing left that knows how to remove it.
	//
	// If you want the other engine, you create a new stack and remove the old one — deliberately,
	// having decided to.
	if want := strings.TrimSpace(req.Engine); want != "" && want != stack.Engine {
		httpx.BadRequest(w, r,
			"A stack's engine cannot be changed. The containers it already made carry the old "+
				"engine's labels, and the new one would never find them — so they would keep "+
				"running with nothing left that knows how to remove them. Create a new stack, and "+
				"remove this one.")
		return
	}

	// Switching the source kind is a deliberate, validated transition — not the silent field
	// rewrite the rest of this handler performs. Only inline → git is offered.
	if req.SourceKind != stack.SourceKind {
		// A git-backed stack's compose lives in the repo, which is the source of truth; there is
		// nothing to import back into an inline_yaml, and snapshotting the last resolved file would
		// silently fork the stack from its repo. Refuse rather than half-do it.
		if stack.SourceKind != "inline" || req.SourceKind != "git" {
			httpx.BadRequest(w, r,
				"Only an inline stack can be switched to git. A git-backed stack keeps its "+
					"compose in the repository, so there is nothing to convert back to inline.")
			return
		}
		req.GitURL = strings.TrimSpace(req.GitURL)
		if req.GitURL == "" {
			httpx.BadRequest(w, r, "A repository URL is required to switch this stack to git.")
			return
		}

		// Pre-flight: prove the new source is actually deployable before we commit the switch —
		// clone the repo, find the compose file at the given ref/path, and parse it (env
		// interpolation included). resolveSource does all three; ls-remote alone would miss a
		// reachable repo that lacks the compose file at that path. A failure here is the operator's
		// mistake, so it is a 400 with the friendly git reason, not a 500.
		probe := *stack
		probe.SourceKind = "git"
		probe.GitURL, probe.GitRef, probe.GitPath = req.GitURL, req.GitRef, req.GitPath
		probe.GitCredentialID = req.GitCredentialID
		if _, _, err := s.resolveSource(r.Context(), &probe); err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "git_unreachable", err.Error())
			return
		}

		// The inline YAML is now stale and would be dead data on a git row. Clear it so the stack
		// has exactly one source of truth.
		req.InlineYAML = ""
	}

	stack.GroupName = strings.TrimSpace(req.GroupName)
	stack.SourceKind = req.SourceKind
	stack.GitURL, stack.GitRef, stack.GitPath = req.GitURL, req.GitRef, req.GitPath
	stack.GitCredentialID = req.GitCredentialID
	stack.InlineYAML = req.InlineYAML

	if err := s.store.UpdateStackSource(r.Context(), stack); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: stack.EnvID, Action: "stack.update", Target: stack.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, viewStack(stack))
}

// handleDeleteStack removes a stack: its containers, its network, and then its record.
//
// It used to remove only the record, and leave the containers running. That was deliberate and
// it was wrong. A warning in a confirm dialog does not make the result good: what you were left
// with was a set of containers that Daffa had forgotten the name of, could no longer manage as a
// unit, and could not offer to clean up — because the thing that knew they were a stack was the
// row that had just been deleted.
//
//	?volumes=true  also removes the stack's named volumes. That destroys data.
//	?force=true    skips the teardown and only forgets the stack.
//
// force exists for one real case: the host is gone. Without it, a stack on a decommissioned
// machine could never be removed from Daffa, because the cleanup could never run. It is the old
// behaviour, kept, but as an explicit request rather than as the default nobody asked for.
func (s *Server) handleDeleteStack(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	userID := ""
	if u != nil {
		userID = u.ID
	}

	var (
		force   = r.URL.Query().Get("force") == "true"
		volumes = r.URL.Query().Get("volumes") == "true"
	)

	// An engine that cannot remove volumes refuses HERE, before anything is attempted — so the
	// answer is "no", not "it did not work". Those are different sentences, and reporting the
	// second when the first is true would tell somebody their delete half-failed when in fact it
	// never started.
	//
	// The UI does not draw the box for such an engine (it renders from Actions()), so this is the
	// backstop for anything that asks anyway.
	if volumes {
		if eng, err := stacks.EngineFor(stack.Engine); err == nil && !stacks.Supports(eng, stacks.ActionDownVolumes) {
			httpx.BadRequest(w, r, eng.Label()+" cannot remove a stack's volumes: they live on each "+
				"node, and the manager has no authority over them. Remove them per node, under Volumes.")
			return
		}
	}

	// Removing a stack's volumes is not "more delete", it is destroying a database. It takes
	// the capability that says so, on this host — the same bit that guards removing a volume by
	// hand, because it is the same act.
	if volumes && !s.mayUseEnv(w, r, caps.VolumesEdit, stack.EnvID) {
		return
	}

	if !force {
		if err := s.tearDown(r.Context(), stack, volumes, userID); err != nil {
			s.audit(r.Context(), store.AuditEntry{
				UserID: userID, EnvID: stack.EnvID, Action: "stack.delete",
				Target: stack.Name, Outcome: "error", Detail: err.Error(),
			})

			switch {
			case errors.Is(err, store.ErrRunInProgress):
				httpx.Fail(w, r, http.StatusConflict, "run_in_progress",
					"This stack has a deploy running. Wait for it to finish, then delete.")
			case errors.Is(err, ErrHostUnreachable):
				// Not a failure of the teardown — there is nowhere to run it. Offer the escape
				// hatch by name, rather than leaving the operator stuck with a stack they
				// cannot remove.
				httpx.Fail(w, r, http.StatusConflict, "host_unreachable",
					"The host this stack runs on is not connected, so its containers cannot be "+
						"removed. You can still remove the stack from Daffa — its containers "+
						"will keep running wherever they are.")
			default:
				httpx.Fail(w, r, http.StatusBadGateway, "teardown_failed",
					"The stack's containers could not be removed, so it has NOT been deleted — "+
						"a stack Daffa cannot clean up is one it must not forget.\n\n"+err.Error())
			}
			return
		}
	}

	if err := s.store.DeleteStack(r.Context(), stack.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}

	action := "stack.delete"
	if force {
		action = "stack.forget" // the containers are still out there; the log should say so
	}
	s.audit(r.Context(), store.AuditEntry{
		UserID: userID, EnvID: stack.EnvID, Action: action, Target: stack.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"volumes_removed": volumes}),
	})
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// stackDetailResponse is everything the stack detail page needs in one answer. The
// optional halves are independent: a broken source still reports the stack (with
// source_error saying why), and an unreachable host still reports the source.
type stackDetailResponse struct {
	Stack stackView `json:"stack"`
	// Services as the compose file DECLARES them; Status.Services is what is running.
	Services []stacks.Service `json:"services,omitempty"`
	YAML     string           `json:"yaml,omitempty"`
	Status   *stacks.Status   `json:"status,omitempty"`
	// Warnings are things that are TRUE about this stack and that nobody asked about —
	// chiefly the swarm node-local volume trap. Not errors: the deploy proceeds.
	Warnings []stacks.Warning `json:"warnings,omitempty"`
	// SourceError is why the source could not be read (bad git ref, invalid YAML). The
	// stack itself is still returned — that is precisely when someone needs to edit it.
	SourceError string `json:"source_error,omitempty"`
}

// handleStackDetail returns everything the detail page needs: the source, the declared
// services, and what is actually running.
func (s *Server) handleStackDetail(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}

	resp := stackDetailResponse{Stack: viewStack(stack)}

	bundle, services, err := s.bundleFor(r.Context(), stack)
	if err != nil {
		// A stack whose source is broken (bad git ref, invalid YAML) must still be
		// viewable and editable — that is precisely when someone needs to fix it.
		resp.SourceError = err.Error()
		httpx.JSON(w, http.StatusOK, resp)
		return
	}
	resp.Services = services
	resp.YAML = bundle.YAML

	// Things that are TRUE about this stack and that nobody asked about. Chiefly: a named volume on
	// a swarm service with no placement constraint is node-local, so if the task is ever
	// rescheduled it finds a fresh empty volume and the database is gone. Silent, and the most
	// expensive Swarm mistake available — so Daffa says it, on the page, before it happens.
	if stack.Engine == stacks.SwarmEngine.Name() {
		resp.Warnings = s.swarmWarnings(r.Context(), stack, bundle)
	}

	eng, err := stacks.EngineFor(stack.Engine)
	if err != nil {
		resp.SourceError = err.Error()
		httpx.JSON(w, http.StatusOK, resp)
		return
	}

	node, err := s.runnerNode(r.Context(), stack)
	if err != nil {
		resp.Status = &stacks.Status{State: "unreachable", Services: []stacks.ServiceStatus{}}
		httpx.JSON(w, http.StatusOK, resp)
		return
	}

	status, err := stacks.Describe(r.Context(), node, eng, stack.Name, services, stack.DeployedHash, bundle.Hash)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp.Status = status
	httpx.JSON(w, http.StatusOK, resp)
}

// swarmWarnings reports what a swarm stack's compose file quietly means. The node count changes the
// wording — on a single-node swarm the trap cannot spring today, but it springs the day a second
// machine is added, which is exactly when nobody will be thinking about it.
func (s *Server) swarmWarnings(ctx context.Context, stack *store.Stack, bundle *stacks.Bundle) []stacks.Warning {
	nodes := 1
	if env, err := s.pool.Get(stack.EnvID); err == nil {
		nodes = len(env.Nodes())
	}
	ws, err := stacks.SwarmWarnings(ctx, bundle.YAML, stack.Name, bundle.Env, nodes)
	if err != nil {
		return nil
	}
	return ws
}

// stack returns the stack this request is about.
//
// The scopeStack middleware already resolved it AND checked that the caller holds the
// route's capability on the stack's host, so this is a context read, not a query. The
// fallback exists only so a handler mounted without that middleware still fails closed
// rather than silently skipping the check.
func (s *Server) stack(w http.ResponseWriter, r *http.Request) (*store.Stack, bool) {
	if st, ok := stackFrom(r.Context()); ok {
		return st, true
	}
	httpx.Fail(w, r, http.StatusNotFound, "no_such_stack", "No such stack.")
	return nil, false
}

// ── env vars ────────────────────────────────────────────────────────────────────

type envVarView struct {
	Key      string `json:"key"`
	Value    string `json:"value"` // "" for a secret that is already saved
	IsSecret bool   `json:"is_secret"`
}

func (s *Server) handleStackEnv(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}
	u, _ := auth.UserFrom(r.Context())

	rows, err := s.store.StackEnv(r.Context(), stack.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]envVarView, 0, len(rows))
	for _, row := range rows {
		v := envVarView{Key: row.Key, IsSecret: row.IsSecret}

		// A secret is write-only once saved, and someone with only stacks.view sees no
		// values at all. The keys are still listed, because knowing WHICH variables a
		// stack takes is not the same as knowing what they are.
		//
		// This is field-level, so it cannot be expressed in the route table: the route
		// needs stacks.view, and the values inside the response need stacks.edit.
		if !row.IsSecret && u != nil && u.Caps.Has(caps.StacksEdit, stack.EnvID) {
			plain, err := s.sealer.Open(row.ValueEnc)
			if err != nil {
				plain = ""
			}
			v.Value = plain
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type setEnvRequest struct {
	Vars []envVarView `json:"vars"`
}

func (s *Server) handleSetStackEnv(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}

	var req setEnvRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	existing, err := s.store.StackEnv(r.Context(), stack.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	prev := map[string]store.StackEnv{}
	for _, e := range existing {
		prev[e.Key] = e
	}

	out := make([]store.StackEnv, 0, len(req.Vars))
	for _, v := range req.Vars {
		v.Key = strings.TrimSpace(v.Key)
		if v.Key == "" {
			continue
		}

		// A secret submitted with an empty value means "unchanged" — the UI never had
		// the plaintext to send back. Without this, opening the page and saving would
		// silently blank every secret the stack has.
		if v.Value == "" && v.IsSecret {
			if old, ok := prev[v.Key]; ok {
				out = append(out, old)
				continue
			}
		}

		sealed, err := s.sealer.Seal(v.Value)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		out = append(out, store.StackEnv{Key: v.Key, ValueEnc: sealed, IsSecret: v.IsSecret})
	}

	if err := s.store.SetStackEnv(r.Context(), stack.ID, out); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: stack.EnvID, Action: "stack.env", Target: stack.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"count": len(out)}), // keys only, never values
	})
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// ── secrets ───────────────────────────────────────────────────────────────────────

// stackSecretView is one stack secret on the wire. Content is WRITE-ONLY: it is accepted on a
// PUT and never returned on a GET. A secret's bytes leave Daffa only as a file in the deploy
// bundle — never in an API response, to stacks.view or stacks.edit. See docs/secrets.md.
type stackSecretView struct {
	Name    string `json:"name"`
	Content string `json:"content,omitempty"`
}

// handleStackSecrets lists a stack's secret NAMES. Never their bytes: a secret is write-only,
// so there is nothing to reveal — knowing WHICH secrets a stack takes is not knowing what they
// are, the same field-level rule as env vars.
func (s *Server) handleStackSecrets(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}

	rows, err := s.store.StackSecrets(r.Context(), stack.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]stackSecretView, 0, len(rows))
	for _, row := range rows {
		out = append(out, stackSecretView{Name: row.Name})
	}
	httpx.JSON(w, http.StatusOK, out)
}

type setSecretsRequest struct {
	Secrets []stackSecretView `json:"secrets"`
}

func (s *Server) handleSetStackSecrets(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}

	// File-based stack secrets are a Swarm feature: `docker stack deploy` reads the file and turns
	// it into a raft secret. A compose stack's `file:` secret would become a bind mount the daemon
	// resolves on the host, but the bundle only exists inside the runner container — so it can never
	// mount. Rather than ship a secret that will fail at deploy, refuse it here and name the path
	// that does work: a secret environment variable, which is sealed and delivered through .env.
	if stack.Engine != stacks.SwarmEngine.Name() {
		httpx.BadRequest(w, r,
			"Stack secrets are a Swarm feature — they become raft secrets. On a compose stack, store "+
				"sensitive values as secret environment variables on the Environment tab instead.")
		return
	}

	var req setSecretsRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	existing, err := s.store.StackSecrets(r.Context(), stack.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	prev := map[string]store.StackSecret{}
	for _, e := range existing {
		prev[e.Name] = e
	}

	seen := map[string]bool{}
	out := make([]store.StackSecret, 0, len(req.Secrets))
	for _, v := range req.Secrets {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			continue
		}
		if !validSecretName(name) {
			httpx.BadRequest(w, r, "A secret name may contain only letters, digits, '.', '_' and '-' — "+
				"it becomes the file mounted at /run/secrets/<name>.")
			return
		}
		if seen[name] {
			httpx.BadRequest(w, r, "Duplicate secret name: "+name)
			return
		}
		seen[name] = true

		// Empty content means "unchanged" — the UI never held the plaintext to send back.
		// Without this, opening the page and saving would blank every secret the stack has.
		if v.Content == "" {
			if old, ok := prev[name]; ok {
				out = append(out, old)
				continue
			}
			httpx.BadRequest(w, r, "Secret "+name+" has no content.")
			return
		}

		sealed, err := s.sealer.Seal(v.Content)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		out = append(out, store.StackSecret{Name: name, ContentEnc: sealed})
	}

	if err := s.store.SetStackSecrets(r.Context(), stack.ID, out); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: stack.EnvID, Action: "stack.secrets", Target: stack.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"count": len(out)}), // names only, never values
	})
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// ── revealing sealed values ──────────────────────────────────────────────────────
//
// The ONE deliberate exception to "sealed values are write-only through the API" (docs/secrets.md).
// It exists because an operator sometimes genuinely needs to read a value back — to hand it to a
// teammate, to check what is actually deployed — and the honest answer is a gated, audited reveal
// rather than making them redeploy to find out or keep the value in a second place.
//
// Three things keep the crack narrow. It needs secrets.reveal, a STANDALONE capability nobody holds
// by default and that stacks.edit does not imply — setting a value and reading every value already
// there are different trusts. Each reveal is ONE value, on demand: the join-token posture, so
// plaintext is fetched by an explicit act, never sitting decrypted on a page left open. And every
// reveal is AUDITED with the key/name (never the value), so the plaintext leaving Daffa is always an
// event with a person's name on it.

// revealedValue is the plaintext of one sealed thing — a secret env var's value or a secret file's
// content, both shaped the same on the wire.
type revealedValue struct {
	Value string `json:"value"`
}

func (s *Server) handleRevealStackEnv(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	rows, err := s.store.StackEnv(r.Context(), stack.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	for _, row := range rows {
		if row.Key != key {
			continue
		}
		plain, err := s.sealer.Open(row.ValueEnc)
		if err != nil {
			httpx.Fail(w, r, http.StatusInternalServerError, "unseal_failed",
				"That value could not be decrypted — was the master key replaced?")
			return
		}
		// Audited before the response: a reveal that reached the client but left no trace is the one
		// failure this whole feature must not have.
		s.audit(r.Context(), store.AuditEntry{
			EnvID: stack.EnvID, Action: "stack.env.reveal", Target: stack.Name, Outcome: "ok",
			Detail: store.AuditDetail(map[string]any{"key": key}), // the key, never the value
		})
		httpx.JSON(w, http.StatusOK, revealedValue{Value: plain})
		return
	}
	httpx.Fail(w, r, http.StatusNotFound, "no_such_var", "This stack has no such variable.")
}

func (s *Server) handleRevealStackSecret(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	rows, err := s.store.StackSecrets(r.Context(), stack.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	for _, row := range rows {
		if row.Name != name {
			continue
		}
		plain, err := s.sealer.Open(row.ContentEnc)
		if err != nil {
			httpx.Fail(w, r, http.StatusInternalServerError, "unseal_failed",
				"That secret could not be decrypted — was the master key replaced?")
			return
		}
		s.audit(r.Context(), store.AuditEntry{
			EnvID: stack.EnvID, Action: "stack.secret.reveal", Target: stack.Name, Outcome: "ok",
			Detail: store.AuditDetail(map[string]any{"name": name}), // the name, never the content
		})
		httpx.JSON(w, http.StatusOK, revealedValue{Value: plain})
		return
	}
	httpx.Fail(w, r, http.StatusNotFound, "no_such_secret", "This stack has no such secret.")
}

// validSecretName keeps a secret name to a safe filename: it becomes daffa-secrets/<name> in
// the bundle and /run/secrets/<name> in the container, so a '/' or '..' could escape the
// directory. stacks.BuildPlanned refuses those too; this is the legible, early refusal.
func validSecretName(name string) bool {
	if name == "" || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ── actions ─────────────────────────────────────────────────────────────────────

func (s *Server) handleStackAction(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}

	eng, err := stacks.EngineFor(stack.Engine)
	if err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	// Whether an action EXISTS is a question for the engine, not for a global list of verbs.
	// Compose can stop a stack; swarm cannot. The UI already renders only what the engine
	// offers, so reaching here with an unsupported action means someone bypassed it — say so
	// plainly instead of failing somewhere down inside the runner.
	action := stacks.Action(r.PathValue("action"))
	if !stacks.Supports(eng, action) {
		httpx.BadRequest(w, r, "The "+eng.Label()+" engine cannot "+string(action)+" a stack.")
		return
	}

	var userID string
	if u, ok := auth.UserFrom(r.Context()); ok {
		userID = u.ID
	}

	dep, err := s.deploy(r.Context(), stack, action, store.TriggerManual, userID, nil)
	switch {
	case errors.Is(err, store.ErrRunInProgress):
		httpx.Fail(w, r, http.StatusConflict, "run_in_progress",
			"A deploy is already running for this stack. Wait for it to finish.")
		return
	case err != nil:
		s.audit(r.Context(), store.AuditEntry{
			EnvID: stack.EnvID, Action: "stack." + string(action), Target: stack.Name,
			Outcome: "error", Detail: store.AuditDetail(map[string]string{"error": err.Error()}),
		})
		httpx.Fail(w, r, http.StatusBadRequest, "deploy_failed", err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, deployStartedResponse{DeploymentID: dep.ID, Status: "running"})
}

// ── registries ──────────────────────────────────────────────────────────────────

type registryView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Username string `json:"username"`
}

func viewRegistry(reg *store.Registry) *registryView {
	return &registryView{ID: reg.ID, Name: reg.Name, URL: reg.URL, Username: reg.Username}
}

func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListRegistries(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Passwords never leave the server, not even sealed.
	out := make([]registryView, 0, len(list))
	for _, reg := range list {
		out = append(out, *viewRegistry(reg))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// managedCAPEMs returns the public PEM of every CA Daffa manages and trusts for its OWN
// outbound TLS — nothing pasted in and no skip-verify. It is the shared basis for both
// registry (registryTrust) and git (managedCABundle) trust, so the two never drift. A CA
// with outbound_trust off is excluded: it exists to be bundled into deliveries (someone
// else's trust anchor), not to widen what the console itself accepts.
func (s *Server) managedCAPEMs(ctx context.Context) []string {
	cas, _ := s.store.ListCertAuthorities(ctx)
	pems := make([]string, 0, len(cas))
	for _, ca := range cas {
		if ca.CertPEM != "" && ca.OutboundTrust {
			pems = append(pems, ca.CertPEM)
		}
	}
	return pems
}

// registryTrust is the CA pool Daffa's own registry reach-out verifies against: the system roots
// plus every CA Daffa itself manages. A registry fronted by a Daffa-issued leaf (the common
// internal case) verifies with zero per-registry config. Returns nil (use the system roots
// directly) when there are no managed CAs, preserving the exact prior behaviour for Docker Hub /
// GHCR and friends.
func (s *Server) registryTrust(ctx context.Context) *x509.CertPool {
	pems := s.managedCAPEMs(ctx)
	if len(pems) == 0 {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, p := range pems {
		pool.AppendCertsFromPEM([]byte(p))
	}
	return pool
}

// managedCABundle is the PEM bundle of Daffa's managed CAs, for go-git's CloneOptions.CABundle —
// which trusts it *in addition to* the system roots, so a self-hosted git server fronted by a
// Daffa-issued cert verifies while public hosts are unaffected. Empty ⇒ nil (system roots only,
// the unchanged public path). Same policy as registryTrust: Daffa's own CAs, nothing else.
func (s *Server) managedCABundle(ctx context.Context) []byte {
	pems := s.managedCAPEMs(ctx)
	if len(pems) == 0 {
		return nil
	}
	bundle, err := certs.Bundle(pems...)
	if err != nil || bundle == "" {
		return nil
	}
	return []byte(bundle)
}

type registryRequest struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Verify defaults to true (nil ⇒ true); when it is on and the probe cannot reach the registry,
	// the response is a soft "unreachable" and the client re-submits with verify:false to store the
	// credential anyway — deploy pulls run from the host daemon, so Daffa's own reach is advisory.
	Verify *bool `json:"verify"`
}

// registryCreateResponse is the create result. On a saved credential Registry is set. When the
// pre-save probe could not reach the registry (and verify was on) Registry is nil and Unreachable
// carries the reason — the client offers "save anyway", which re-POSTs with verify:false. Deploy
// pulls run from the host daemon, so Daffa's own unreachability is never a reason to refuse a save.
type registryCreateResponse struct {
	Registry    *registryView `json:"registry,omitempty"`
	Unreachable bool          `json:"unreachable,omitempty"`
	Reason      string        `json:"reason,omitempty"`
}

func (s *Server) handleCreateRegistry(w http.ResponseWriter, r *http.Request) {
	var req registryRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name, req.URL = strings.TrimSpace(req.Name), strings.TrimSpace(req.URL)
	if req.Name == "" || req.URL == "" {
		httpx.BadRequest(w, r, "A name and a URL are required.")
		return
	}
	// Store the bare, normalised host — no scheme, no path, lowercased — so matching a stack's
	// image hosts against stored credentials at deploy time is a plain string compare. The probe
	// below still uses exactly what the operator typed, so an http:// prefix on a plain-HTTP
	// registry is honoured there even though it is not part of what we keep.
	host := dockerx.RegistryHost(req.URL)
	if host == "" {
		httpx.BadRequest(w, r, "That does not look like a registry host.")
		return
	}
	// A registry credential with no secret authenticates nothing — anonymous pulls need no entry
	// at all. Requiring the password also keeps the pre-save check honest: it has a credential to
	// actually try.
	if req.Password == "" {
		httpx.BadRequest(w, r, "A password or token is required — a registry credential with neither authenticates nothing.")
		return
	}

	// Prove the credential works before saving it — a wrong password otherwise stays silent until a
	// deploy tries to pull a private image. But the probe runs from THE DAFFA CONTAINER, and the
	// actual pull runs from the host daemon; a registry the host can reach may be unresolvable or
	// untrusted from here (the genie/Tailscale case). So on verify:true the probe is ADVISORY — a
	// failure returns a soft "couldn't reach it, save anyway?" rather than a refusal. verify:false
	// skips it entirely. See .ai/registries.md §Design #3.
	verify := req.Verify == nil || *req.Verify
	if verify {
		if err := dockerx.CheckRegistry(r.Context(), req.URL, req.Username, req.Password, s.registryTrust(r.Context())); err != nil {
			// A wrong username/password is actionable and fails the host daemon too, so it stays a
			// hard error. Only a registry Daffa cannot reach becomes the soft "save anyway" — the
			// host may still be able to pull from it.
			if errors.Is(err, dockerx.ErrBadCredential) {
				httpx.Fail(w, r, http.StatusBadRequest, "registry_unauthorized", err.Error())
				return
			}
			httpx.JSON(w, http.StatusOK, registryCreateResponse{Unreachable: true, Reason: err.Error()})
			return
		}
	}

	sealed, err := s.sealer.Seal(req.Password)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	reg := &store.Registry{Name: req.Name, URL: host, Username: req.Username, PasswordEnc: sealed}
	if err := s.store.CreateRegistry(r.Context(), reg); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "A registry with that name already exists.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "registry.create", Target: reg.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, registryCreateResponse{Registry: viewRegistry(reg)})
}

func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteRegistry(r.Context(), r.PathValue("id")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "registry.delete", Target: r.PathValue("id"), Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
