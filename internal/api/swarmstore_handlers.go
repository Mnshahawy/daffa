package api

import (
	"net/http"
	"strings"

	"github.com/Mnshahawy/daffa/internal/httpx"
)

// The cluster's own existence.
//
// Swarm secrets and configs used to live here as free-floating cluster objects. They are retired:
// a secret is now a STACK's sealed sub-resource, delivered as a bundle file on every engine
// (docs/secrets.md), and config lives in git behind a volume source (docs/volumes.md). What is left
// is swarm init, join tokens and leave — the cluster's existence, which belongs to no stack.

// ── the cluster's own existence ─────────────────────────────────────────────────

type swarmInitRequest struct {
	// AdvertiseAddr is which of this machine's addresses the other machines should dial. Empty means
	// "work it out", which Docker does correctly on a machine with one obvious address and REFUSES
	// to guess on a machine with several. That refusal is right, and its error message says so, so
	// it is passed straight through rather than us picking an interface and being wrong on exactly
	// the machine where it matters.
	AdvertiseAddr string `json:"advertise_addr"`
}

// swarmInitResponse carries the new cluster's one fact: the manager's swarm node id.
type swarmInitResponse struct {
	NodeID string `json:"node_id"`
}

// handleSwarmInit turns a standalone host into a single-node Swarm.
//
// It is the ONE swarm operation that runs against a daemon which is not yet a manager — it is what
// makes it one — so it is the one place s.control() cannot be used to find its target. It uses
// s.node() instead, and the asymmetry is the point rather than an exception to it.
func (s *Server) handleSwarmInit(w http.ResponseWriter, r *http.Request) {
	env, ok := s.env(w, r)
	if !ok {
		return
	}
	if env.IsSwarm() {
		httpx.Fail(w, r, http.StatusConflict, "already_a_swarm",
			"This environment is already a Swarm cluster.")
		return
	}

	node, ok := s.node(w, r)
	if !ok {
		return
	}

	var req swarmInitRequest
	if r.ContentLength > 0 {
		if err := httpx.Decode(w, r, &req); err != nil {
			httpx.BadRequest(w, r, err.Error())
			return
		}
	}

	id, err := node.SwarmInit(r.Context(), strings.TrimSpace(req.AdvertiseAddr))
	s.auditResource(r, node.EnvID, "swarm.init", node.Name, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "init_failed", err.Error())
		return
	}

	// Learn what it just became, now, rather than up to a minute from now when the liveness sweep
	// gets round to it. An operator who has just created a Swarm should not be told they are on a
	// standalone host.
	s.reconcileEnv(r.Context(), node.EnvID)

	httpx.JSON(w, http.StatusOK, swarmInitResponse{NodeID: id})
}

// handleJoinTokens returns the CREDENTIALS that let a machine join the cluster.
//
// This is the only route that serves them, and it requires swarm.edit. Docker hands the tokens back
// from GET /swarm alongside a lot of harmless information, which means anything that inspects a
// swarm and forwards the result is one careless line from leaking them — Portainer strips them from
// that response for non-admins. We never put them in a shared payload at all: they are not on the
// node list, not on the environment, not on any inspect a services.view holder can make. One route,
// one capability, nothing else.
func (s *Server) handleJoinTokens(w http.ResponseWriter, r *http.Request) {
	control, ok := s.control(w, r)
	if !ok {
		return
	}

	tokens, err := control.SwarmJoinTokens(r.Context())
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "tokens_failed", err.Error())
		return
	}

	// Reading a join token is reading a credential, so it is audited as an event in its own right —
	// not merely as a page view. "Who has the token" is a question somebody will one day need to
	// answer.
	s.auditResource(r, control.EnvID, "swarm.tokens.read", control.Name, nil)
	httpx.JSON(w, http.StatusOK, tokens)
}

// handleSwarmLeave takes a node out of its Swarm.
//
// For the LAST MANAGER this dissolves the cluster: the raft store goes, and with it every service,
// secret and config definition. The containers keep running until something stops them, which makes
// the damage quiet as well as total — so Docker demands `force`, and so do we, and the UI says what
// it destroys rather than asking "are you sure?".
func (s *Server) handleSwarmLeave(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "true"

	err := node.SwarmLeave(r.Context(), force)
	s.auditResource(r, node.EnvID, "swarm.leave", node.Name, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "leave_failed", err.Error())
		return
	}

	// It is standalone now. Say so immediately.
	s.reconcileEnv(r.Context(), node.EnvID)

	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}
