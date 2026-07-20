package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// envNodeView is one machine of an environment, as the host switcher renders it.
type envNodeView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // local | agent
	Status string `json:"status"`
}

// envView is an environment as the switcher sees it: standalone or a swarm, made of nodes.
// `swarm` is derived from the swarm id rather than stored, so there is no second copy of
// it to fall out of step.
type envView struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Swarm  bool          `json:"swarm"`
	Status string        `json:"status"`
	Nodes  []envNodeView `json:"nodes"`
}

// handleListEnvironments is the load-bearing filter of the whole scoped model.
//
// A host you hold nothing on does not appear here, and therefore does not appear in the host
// switcher, and therefore is not a place the console can be pointed at. Everything else
// follows from this one list — which is why it filters rather than merely gating.
func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}

	envs, err := s.store.ListEnvironments(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]envView, 0, len(envs))
	for _, e := range envs {
		// clusters.view is the visibility bit. Held globally it shows every cluster; held on one
		// it shows that one. Someone who holds it nowhere sees an empty list and a console with
		// nothing to point at — which is the correct rendering of "you have no access", not a bug.
		if !u.Caps.Has(caps.ClustersView, e.ID) {
			continue
		}

		nodes, err := s.store.NodesByEnv(r.Context(), e.ID)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}

		// Status is live, not remembered: a daemon that died since startup should show as offline
		// without waiting for a background sweep. It is asked of EVERY node, because an
		// environment is only as reachable as the daemons in it — and an environment with one node
		// down out of three is not "offline", it is degraded, which the node list shows and a
		// single flag never could.
		view := envView{ID: e.ID, Name: e.Name, Swarm: e.IsSwarm(), Status: "offline"}
		for _, n := range nodes {
			status := "offline"
			if env, err := s.pool.Get(e.ID); err == nil {
				if node, err := env.Node(n.ID); err == nil {
					if err := node.Ping(r.Context()); err == nil {
						status = "online"
					}
				}
			}
			if status == "online" {
				view.Status = "online"
			}
			view.Nodes = append(view.Nodes, envNodeView{ID: n.ID, Name: n.Name, Kind: n.Kind, Status: status})
		}
		out = append(out, view)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type renameRequest struct {
	Name string `json:"name"`
}

// renameResponse echoes the new identity back, so the switcher can update without refetching.
type renameResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// handleRenameEnvironment lets an admin name a host. "Local" and "agent-3" are what
// Daffa can come up with on its own; "web-1" and "prod-eu" are what an operator actually
// thinks in.
func (s *Server) handleRenameEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cluster")

	env, err := s.store.EnvironmentByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_environment", "No such environment.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req renameRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 40 {
		httpx.BadRequest(w, r, "A name is required (up to 40 characters).")
		return
	}

	// Two hosts with the same name in the switcher is a way to restart the wrong one.
	if existing, err := s.store.EnvironmentByName(r.Context(), req.Name); err == nil && existing.ID != id {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "Another host already has that name.")
		return
	}

	if err := s.store.RenameEnvironment(r.Context(), id, req.Name); err != nil {
		httpx.Error(w, r, err)
		return
	}
	// The pool caches the name for logging; keep it in step.
	s.pool.Rename(id, req.Name)

	s.audit(r.Context(), store.AuditEntry{
		EnvID: id, Action: "environment.rename", Target: req.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"from": env.Name, "to": req.Name}),
	})
	httpx.JSON(w, http.StatusOK, renameResponse{ID: id, Name: req.Name})
}

// env resolves the {cluster} path segment to a live handle, writing the error response
// itself if it cannot.
func (s *Server) env(w http.ResponseWriter, r *http.Request) (*dockerx.Env, bool) {
	env, err := s.pool.Get(r.PathValue("cluster"))
	if err != nil {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_environment",
			"That environment is not connected.")
		return nil, false
	}
	return env, true
}

// node and control are the two ways a handler reaches a daemon, and CHOOSING BETWEEN THEM IS THE
// WHOLE OF THE NODE-LOCAL / CLUSTER-WIDE SPLIT (docs/swarm.md §3).
//
// Docker draws the line and will not tell you where it is: a manager answers "list the containers"
// (about itself, only) and "list the services" (about the whole cluster) through the same socket,
// with no hint that one of those answers is local and the other is not. Get it wrong and you do not
// get an error — you get a confident, wrong answer, which is worse.
//
// Portainer draws the same line with an HTTP header (X-PortainerAgent-ManagerOperation) that a
// caller can simply forget to set. Here it is two functions, and a handler that reaches for the
// wrong daemon has to have called the wrong one by name. Same instinct as caps.Cap being a struct
// rather than an int: make the mistake unrepresentable, do not document it.

// node resolves the daemon a NODE-LOCAL request is about: containers, images, volumes, exec,
// stats, prune. Everything, in other words, that belongs to one machine.
//
// The ?node= parameter names it — and is required only when there is more than one node to choose
// between. The rule is ARITY, not kind: a standalone environment has one node, and so does a
// single-node swarm, which is the topology most people actually run. A parameter becomes required
// at exactly the moment it becomes meaningful.
func (s *Server) node(w http.ResponseWriter, r *http.Request) (*dockerx.Node, bool) {
	env, ok := s.env(w, r)
	if !ok {
		return nil, false
	}

	if id := r.URL.Query().Get("node"); id != "" {
		n, err := env.Node(id)
		if err != nil {
			httpx.Fail(w, r, http.StatusNotFound, "no_such_node",
				"That environment has no such node, or Daffa cannot reach it.")
			return nil, false
		}
		return n, true
	}

	n, err := env.One()
	if err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "node_required",
			"This environment has more than one node, so this request has to say which one it means.")
		return nil, false
	}
	return n, true
}

// nodeForContainer is `node` for a request that names a CONTAINER, with one extra move: when the
// caller did not pass ?node= and the environment has more than one node, it finds the node that
// actually runs the container in the path rather than refusing with node_required. A container id
// is unique per daemon, so the first node that can inspect it is the right one.
//
// This is what lets a caller that cannot know the node still reach the right daemon — the container
// list's stats, a COMPOSE stack's panel on a multi-node host — while `node` keeps refusing for the
// requests that have no id to resolve (prune, df), where naming the node is the only honest answer.
// A single-node environment still short-circuits with no lookup, so the common case pays nothing.
func (s *Server) nodeForContainer(w http.ResponseWriter, r *http.Request) (*dockerx.Node, bool) {
	env, ok := s.env(w, r)
	if !ok {
		return nil, false
	}

	if id := r.URL.Query().Get("node"); id != "" {
		n, err := env.Node(id)
		if err != nil {
			httpx.Fail(w, r, http.StatusNotFound, "no_such_node",
				"That environment has no such node, or Daffa cannot reach it.")
			return nil, false
		}
		return n, true
	}

	// One node (standalone or single-node swarm): no ambiguity, no lookup.
	if n, err := env.One(); err == nil {
		return n, true
	}

	// Many nodes, none named: locate the one that runs this container. A successful inspect is
	// proof of ownership; ask each daemon in turn until one answers.
	if cid := r.PathValue("id"); cid != "" {
		for _, n := range env.Nodes() {
			if _, err := n.InspectContainer(r.Context(), cid); err == nil {
				return n, true
			}
		}
	}
	httpx.Fail(w, r, http.StatusNotFound, "no_such_container",
		"No such container on any reachable node in this environment.")
	return nil, false
}

// control resolves the daemon a CLUSTER-WIDE request is about: services, tasks, swarm nodes,
// secrets, configs, stack deploys. A manager — the leader if it is reachable, otherwise any of
// them.
//
// It fails on a standalone environment, because there is no cluster to ask about, and it fails on a
// swarm whose managers Daffa cannot reach. That second case is real rather than defensive: enroll
// an agent on a worker and you have a swarm environment with no control node. Node-local work on it
// still works perfectly. Cluster-wide work has nobody to ask, and saying so beats returning an
// empty list, which is a lie shaped like an answer.
func (s *Server) control(w http.ResponseWriter, r *http.Request) (*dockerx.Node, bool) {
	env, ok := s.env(w, r)
	if !ok {
		return nil, false
	}

	if !env.IsSwarm() {
		httpx.Fail(w, r, http.StatusConflict, "not_a_swarm",
			"This environment is a standalone host, not a Swarm cluster.")
		return nil, false
	}

	n, err := env.Control()
	if err != nil {
		httpx.Fail(w, r, http.StatusConflict, "no_swarm_manager",
			"Daffa cannot reach a manager in this Swarm, so it cannot answer questions about the cluster.")
		return nil, false
	}
	return n, true
}

func (s *Server) handleEnvInfo(w http.ResponseWriter, r *http.Request) {
	env, ok := s.env(w, r)
	if !ok {
		return
	}

	// A specific ?node= wins; otherwise, rather than refuse a multi-node cluster for want of one,
	// answer for its manager — the daemon a cluster-level question is really about. A standalone
	// cluster has one node, so this is that node.
	var node *dockerx.Node
	if id := r.URL.Query().Get("node"); id != "" {
		n, err := env.Node(id)
		if err != nil {
			httpx.Fail(w, r, http.StatusNotFound, "no_such_node",
				"That cluster has no such node, or Daffa cannot reach it.")
			return
		}
		node = n
	} else if n, err := env.Control(); err == nil {
		node = n
	} else if n, err := env.One(); err == nil {
		node = n
	} else {
		httpx.Fail(w, r, http.StatusBadGateway, "no_reachable_node",
			"Daffa cannot reach a daemon for this cluster.")
		return
	}

	info, err := node.Info(r.Context())
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable",
			"Could not reach the Docker daemon for this environment.")
		return
	}
	httpx.JSON(w, http.StatusOK, info)
}

func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	env, ok := s.env(w, r)
	if !ok {
		return
	}

	all := r.URL.Query().Get("all") != "false" // default: show stopped containers too
	list, err := fanOutErr(r.Context(), env,
		func(ctx context.Context, n *dockerx.Node) ([]dockerx.Container, error) {
			return n.ListContainers(ctx, all)
		},
		func(c *dockerx.Container, n *dockerx.Node) { c.Node, c.NodeID = n.Name, n.ID })
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (s *Server) handleInspectContainer(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeForContainer(w, r)
	if !ok {
		return
	}

	info, err := node.InspectContainer(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_container", "No such container.")
		return
	}
	httpx.JSON(w, http.StatusOK, info)
}

func (s *Server) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeForContainer(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	action := dockerx.Action(r.PathValue("action"))
	if !dockerx.ValidAction(action) {
		httpx.BadRequest(w, r, "Unknown container action.")
		return
	}
	force := r.URL.Query().Get("force") == "true"

	err := node.DoAction(r.Context(), id, action, force)

	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: node.EnvID, Action: "container." + string(action), Target: id, Outcome: outcome,
		Detail: store.AuditDetail(map[string]any{"force": force, "error": errText(err)}),
	})

	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "action_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// handleContainerLogs streams logs as SSE. The client sets follow=false to fetch a
// tail and stop, or follow=true (default) to keep the stream open.
func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeForContainer(w, r)
	if !ok {
		return
	}

	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "200"
	}
	if n, err := strconv.Atoi(tail); err != nil || n < 0 || n > 10000 {
		httpx.BadRequest(w, r, "tail must be a number between 0 and 10000.")
		return
	}
	follow := r.URL.Query().Get("follow") != "false"

	sse, err := httpx.NewSSE(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	ctx := r.Context()
	err = node.StreamLogs(ctx, r.PathValue("id"), tail, follow, func(line dockerx.LogLine) error {
		return sse.Send("log", line)
	})
	if err != nil && ctx.Err() == nil {
		_ = sse.Send("error", map[string]string{"message": err.Error()})
		return
	}
	if !follow {
		_ = sse.Send("end", map[string]string{"status": "complete"})
	}
}

// dockerEvent is one daemon event, reduced to what query invalidation needs.
type dockerEvent struct {
	Action string `json:"action"`
	ID     string `json:"id"`
	Name   string `json:"name"`
}

// handleEvents relays the daemon's event stream so the UI can invalidate exactly what
// changed instead of polling every list on a timer.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}

	sse, err := httpx.NewSSE(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	ctx := r.Context()
	err = node.Events(ctx, func(action, actorID, actorName string) error {
		return sse.Send("docker", dockerEvent{Action: action, ID: actorID, Name: actorName})
	})
	if err != nil && ctx.Err() == nil {
		_ = sse.Send("error", map[string]string{"message": err.Error()})
	}
}

// auditEntryView is one audit row. The user is the denormalised label written at the time
// of the action, so the name survives the account; detail is a JSON object, as text.
type auditEntryView struct {
	At      time.Time `json:"at"`
	User    string    `json:"user"`
	Action  string    `json:"action"`
	Target  string    `json:"target"`
	Outcome string    `json:"outcome"` // ok | error | denied
	Detail  string    `json:"detail"`
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// Entries with no host — users, roles, identity providers — are fleet-level and are
	// shown only to a GLOBAL audit.view holder. The filter runs in SQL; see ListAudit.
	global, envs := visible(r, caps.AuditView)
	entries, err := s.store.ListAudit(r.Context(), limit, global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]auditEntryView, 0, len(entries))
	for _, e := range entries {
		out = append(out, auditEntryView{
			At: e.At, User: e.UserLabel, Action: e.Action,
			Target: e.Target, Outcome: e.Outcome, Detail: e.Detail,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}
