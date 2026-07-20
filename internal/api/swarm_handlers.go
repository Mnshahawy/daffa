package api

import (
	"net/http"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
)

// Every handler here reaches its daemon through s.control(), never s.node(). That is not a
// convention to remember — it is the whole node-local/cluster-wide split, made structural. A
// manager answers "list the containers" (about itself, only) and "list the services" (about the
// entire cluster) through the same socket and will not tell you which kind of answer it just gave.
// Asking the wrong daemon does not fail; it returns a confident, wrong answer.

func (s *Server) handleListServices(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	services, err := control.ListServices(r.Context())
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable",
			"Could not reach a Swarm manager for this environment.")
		return
	}
	httpx.JSON(w, http.StatusOK, services)
}

func (s *Server) handleInspectService(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	svc, err := control.InspectService(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_service", "No such service.")
		return
	}
	httpx.JSON(w, http.StatusOK, svc)
}

// taskView is one task with Daffa's two annotations on top of what swarm says.
type taskView struct {
	dockerx.Task
	// Node is the machine this task is on, named as a person would say it rather than as a
	// swarm id. Empty when the task has not been placed — which is itself the answer.
	Node string `json:"node,omitempty"`
	// Reachable reports whether Daffa has an agent on that machine. When it does not, there is
	// no shell and no stats for this task, and saying so here is what stops the UI offering a
	// button that cannot work.
	Reachable bool `json:"reachable"`
}

// handleListTasks is the page worth getting right.
//
// A service that says 0/3 tells you nothing. The task underneath says "no suitable node
// (insufficient memory on 2 nodes)" — that is the entire answer, it lives in Task.Status.Err, and
// both Portainer and Dokploy bury it.
//
// Each task is also told whether Daffa can REACH the node it runs on, because the question that
// follows "why is this task failing" is always "let me get a shell on it" — and the honest time to
// answer that is now, on the row, rather than when somebody clicks a button that then errors.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	env, ok := s.env(w, r)
	if !ok {
		return
	}
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	tasks, err := control.ListTasks(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_service", "No such service.")
		return
	}

	swarmNodes, _ := control.ListSwarmNodes(r.Context())
	hostname := map[string]string{}
	for _, n := range swarmNodes {
		hostname[n.ID] = n.Hostname
	}

	out := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		v := taskView{Task: t, Node: hostname[t.NodeID]}
		if t.NodeID != "" {
			_, v.Reachable = env.NodeBySwarmID(t.NodeID)
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// handleServiceLogs follows a service's logs across the whole cluster.
//
// This is the one cluster-wide stream Docker proxies for us — the manager collects from every node
// running a task — so it works with no agent on the workers at all. It reuses the container-log SSE
// path verbatim, because from the browser's point of view a log is a log.
func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	sse, err := httpx.NewSSE(w, r)
	if err != nil {
		httpx.Fail(w, r, http.StatusInternalServerError, "sse_unsupported", "Streaming is not available.")
		return
	}

	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "200"
	}
	follow := r.URL.Query().Get("follow") == "true"

	err = control.ServiceLogs(r.Context(), r.PathValue("id"), tail, follow,
		func(line dockerx.LogLine) error { return sse.Send("log", line) },
		// A node the manager cannot reach is a WARNING, not a failure: the reachable tasks' logs
		// still arrive. It rides its own event so the client shows a notice, not a broken stream.
		func(msg string) error { return sse.Send("warn", map[string]string{"message": msg}) },
	)
	if err != nil && r.Context().Err() == nil {
		_ = sse.Send("error", map[string]string{"message": err.Error()})
	}
}

// clusterNodeView is one machine of the node table: the JOIN of what the Swarm says
// exists against what Daffa can actually reach.
type clusterNodeView struct {
	// From Daffa: can we talk to this machine at all?
	NodeID    string `json:"node_id,omitempty"` // Daffa's node id; empty ⇒ no agent here
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`

	// From the Swarm. Absent for a node Daffa can reach but the swarm has never heard of —
	// which is what a stale row looks like, and it is worth showing rather than hiding.
	SwarmNodeID  string `json:"swarm_node_id,omitempty"`
	Role         string `json:"role,omitempty"`
	Availability string `json:"availability,omitempty"`
	State        string `json:"state,omitempty"`
	Leader       bool   `json:"leader"`
	Version      string `json:"version,omitempty"`
	CPUs         int64  `json:"cpus,omitempty"`
	Memory       int64  `json:"memory,omitempty"`
	InSwarm      bool   `json:"in_swarm"`
}

// handleListNodes is THE JOIN, and the join is the whole value of the page.
//
// One list is what the Swarm says its machines are. The other is what Daffa can actually reach.
// Portainer has both and never reconciles them for the user, so "why can't I get a shell on this
// task?" stays a question. Here it is a sentence on the row, before you click:
//
//	node-4 has no Daffa agent — its containers are not listed and it has no shell.
//
// Neither side is silently dropped. A swarm node with no agent is listed and named as unreachable;
// a Daffa node whose swarm membership has vanished is listed and named as stale.
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	env, ok := s.env(w, r)
	if !ok {
		return
	}

	out := []clusterNodeView{}
	seen := map[string]bool{} // by swarm node id

	// A standalone environment has no swarm to ask. Its "node table" is simply the machine it is,
	// which is still worth rendering — the Environment page is where you go to find out what you
	// are pointed at.
	if env.IsSwarm() {
		control, ok := s.control(w, r)
		if !ok {
			return
		}
		swarmNodes, err := control.ListSwarmNodes(r.Context())
		if err != nil {
			httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable",
				"Could not reach a Swarm manager for this environment.")
			return
		}

		for _, sn := range swarmNodes {
			seen[sn.ID] = true
			v := clusterNodeView{
				Name: sn.Hostname, SwarmNodeID: sn.ID, Role: sn.Role,
				Availability: sn.Availability, State: sn.State, Leader: sn.Leader,
				Version: sn.Version, CPUs: sn.CPUs, Memory: sn.Memory, InSwarm: true,
			}
			// Do we have an agent there? This is the line that answers the shell question.
			if n, ok := env.NodeBySwarmID(sn.ID); ok {
				v.NodeID, v.Name, v.Reachable = n.ID, n.Name, true
			}
			out = append(out, v)
		}
	}

	// Anything Daffa can reach that the swarm did not account for. On a standalone environment
	// that is simply its one node; on a swarm it is a row that has gone stale, and saying so beats
	// quietly dropping a machine somebody believes is being managed.
	for _, n := range env.Nodes() {
		if n.SwarmNodeID != "" && seen[n.SwarmNodeID] {
			continue
		}
		out = append(out, clusterNodeView{
			NodeID: n.ID, Name: n.Name, Reachable: true,
			SwarmNodeID: n.SwarmNodeID, InSwarm: false,
		})
	}

	httpx.JSON(w, http.StatusOK, out)
}

// ── operations ──────────────────────────────────────────────────────────────────

type scaleRequest struct {
	Replicas *uint64 `json:"replicas"`
}

func (s *Server) handleScaleService(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	var req scaleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	// A pointer, not a plain uint64, so that "scale to 0" is distinguishable from "you forgot to
	// say". Scaling to zero is a real thing somebody means; a missing field is not.
	if req.Replicas == nil {
		httpx.BadRequest(w, r, "Say how many replicas you want.")
		return
	}

	id := r.PathValue("id")
	err := control.ScaleService(r.Context(), id, *req.Replicas)
	s.auditResource(r, control.EnvID, "service.scale", id, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "scale_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// handleRedeployService is `docker service update --force`: recreate every task even though nothing
// in the spec changed, re-resolving the image against the registry on the way. It is the only way to
// get new bytes for a floating tag without editing anything.
func (s *Server) handleRedeployService(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	err := control.RedeployService(r.Context(), id)
	s.auditResource(r, control.EnvID, "service.redeploy", id, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "redeploy_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// handleRollbackService puts back the service's PREVIOUS spec, which swarm keeps for exactly this.
//
// It rolls back ONE service. Rolling back a STACK is a different act — it re-applies the whole
// compose file a past deployment stored — and it lives on the deployment, where the file is.
func (s *Server) handleRollbackService(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	err := control.RollbackService(r.Context(), id)
	s.auditResource(r, control.EnvID, "service.rollback", id, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "rollback_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

func (s *Server) handleRemoveService(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	err := control.RemoveService(r.Context(), id)
	s.auditResource(r, control.EnvID, "service.remove", id, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "remove_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

type nodeUpdateRequest struct {
	Availability string `json:"availability"` // active | pause | drain
	Role         string `json:"role"`         // manager | worker
}

// handleUpdateNode changes a machine's availability or its role.
//
// {id} here is the SWARM node id, not Daffa's — this is a swarm operation, addressed the way swarm
// addresses it. The node table sends both, so the caller always has it.
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	var req nodeUpdateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	id := r.PathValue("id")

	switch {
	case req.Availability != "":
		err := control.SetNodeAvailability(r.Context(), id, req.Availability)
		s.auditResource(r, control.EnvID, "node."+req.Availability, id, err)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadGateway, "node_update_failed", err.Error())
			return
		}
	case req.Role != "":
		err := control.SetNodeRole(r.Context(), id, req.Role)
		s.auditResource(r, control.EnvID, "node.role", id, err)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadGateway, "node_update_failed", err.Error())
			return
		}
	default:
		httpx.BadRequest(w, r, "Say what to change: an availability, or a role.")
		return
	}

	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// handleRemoveNode takes a machine out of the swarm's records. It does NOT reach the machine —
// `docker swarm leave` is what a node runs on itself — so this is for the one that is already gone.
func (s *Server) handleRemoveNode(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	force := r.URL.Query().Get("force") == "true"

	err := control.RemoveNode(r.Context(), id, force)
	s.auditResource(r, control.EnvID, "node.remove", id, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "node_remove_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}
