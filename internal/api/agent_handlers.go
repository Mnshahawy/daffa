package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
	"github.com/Mnshahawy/daffa/internal/tunnel"
)

// The agent endpoints are the only ones a machine (rather than a person) authenticates
// to, and the only ones outside the session cookie. They are:
//
//	POST /agents/enroll   — redeem a one-time join token for a long-lived agent token
//	GET  /agents/connect  — open the tunnel, authenticated by that agent token
//
// Both live outside /api/ deliberately: they are not part of the browser's API surface
// and must not be reachable with a session cookie.

const joinTokenTTL = 30 * time.Minute

// registry tracks the agents currently connected. It is memory-only: a tunnel does not
// survive a restart, and pretending otherwise would leave environments showing "online"
// while nothing is on the other end.
type registry struct {
	mu       sync.RWMutex
	sessions map[string]*yamux.Session // by agent id
}

func newRegistry() *registry {
	return &registry{sessions: map[string]*yamux.Session{}}
}

func (r *registry) add(agentID string, s *yamux.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A reconnect from the same agent supersedes the old tunnel — which is what
	// happens after a network blip, where the server has not yet noticed the old one
	// is dead.
	if old, ok := r.sessions[agentID]; ok {
		_ = old.Close()
	}
	r.sessions[agentID] = s
}

// get returns an agent's live tunnel, if it has one.
func (r *registry) get(agentID string) (*yamux.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[agentID]
	return s, ok
}

func (r *registry) remove(agentID string, s *yamux.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Only remove if it is still OUR session: a reconnect may already have replaced it,
	// and the losing goroutine must not evict the winner.
	if cur, ok := r.sessions[agentID]; ok && cur == s {
		delete(r.sessions, agentID)
	}
}

// ── enrollment ──────────────────────────────────────────────────────────────────

type enrollRequest struct {
	Token   string `json:"token"`
	Version string `json:"version"`
}

func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if req.Token == "" {
		httpx.BadRequest(w, r, "A join token is required.")
		return
	}

	agent, err := s.store.RedeemJoinToken(r.Context(), auth.HashToken(req.Token))
	if errors.Is(err, store.ErrNotFound) {
		s.audit(r.Context(), store.AuditEntry{
			Action: "agent.enroll", Outcome: "denied",
			Detail: store.AuditDetail(map[string]string{"reason": "invalid_or_expired_token", "ip": s.clientIP(r)}),
		})
		httpx.Fail(w, r, http.StatusForbidden, "invalid_token",
			"This join token is invalid, expired, or has already been used.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The agent token is generated here and shown exactly once, in this response. We
	// store only its hash — so a compromised database yields no agent credentials.
	token, err := randomToken()
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.store.SetAgentToken(r.Context(), agent.ID, auth.HashToken(token)); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "agent.enroll", Target: agent.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"agent_id": agent.ID, "ip": s.clientIP(r)}),
	})
	slog.Info("agent enrolled", "agent", agent.Name, "id", agent.ID)

	httpx.JSON(w, http.StatusOK, map[string]string{
		"agent_id":   agent.ID,
		"agent_name": agent.Name,
		"token":      token,
	})
}

// ── tunnel ──────────────────────────────────────────────────────────────────────

func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "An agent token is required.")
		return
	}

	agent, err := s.store.AgentByToken(r.Context(), auth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		s.audit(r.Context(), store.AuditEntry{
			Action: "agent.connect", Outcome: "denied",
			Detail: store.AuditDetail(map[string]string{"reason": "unknown_token", "ip": s.clientIP(r)}),
		})
		httpx.Fail(w, r, http.StatusUnauthorized, "invalid_token", "Unknown agent token.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// An agent is not a browser and sends no Origin. Skipping the check here is
		// safe precisely because this endpoint is bearer-authenticated and rejects the
		// session cookie: a hostile page cannot forge an agent token.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled, // Docker payloads are mostly already-compressed layers and JSON we stream; deflate would burn CPU on both ends
	})
	if err != nil {
		slog.Warn("agent websocket upgrade failed", "agent", agent.Name, "err", err)
		return
	}

	// No read limit: this carries image pulls and log streams, not chat messages.
	ws.SetReadLimit(-1)

	session, err := tunnel.Server(ws)
	if err != nil {
		slog.Error("starting agent tunnel", "agent", agent.Name, "err", err)
		_ = ws.Close(websocket.StatusInternalError, "tunnel failed")
		return
	}
	defer session.Close()

	version := r.URL.Query().Get("version")
	ctx := context.WithoutCancel(r.Context())

	env, node, err := s.connectAgent(ctx, agent, version, session)
	if err != nil {
		slog.Error("registering agent environment", "agent", agent.Name, "err", err)
		return
	}

	slog.Info("agent connected", "agent", agent.Name, "env", env.ID, "version", version)
	s.audit(ctx, store.AuditEntry{
		EnvID: env.ID, Action: "agent.connect", Target: agent.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"ip": s.clientIP(r), "version": version}),
	})

	// Block until the tunnel dies. yamux's keepalive detects a half-open connection
	// (a laptop that slept, a NAT that forgot us) without us writing a heartbeat loop.
	<-session.CloseChan()

	s.disconnectAgent(agent.ID, env.ID, node.ID, session)
	slog.Info("agent disconnected", "agent", agent.Name)
	s.audit(ctx, store.AuditEntry{
		EnvID: env.ID, Action: "agent.disconnect", Target: agent.Name, Outcome: "ok",
	})
}

// connectAgent brings a live tunnel into the pool.
//
// An agent enrolls a NODE, not an environment — one machine, one daemon. A freshly enrolled node
// lands in its own standalone environment; if it turns out to be part of a Swarm, that is
// discovered afterwards from what its daemon says about itself, never from what the enrolling side
// asserted. The daemon is the only thing that actually knows.
func (s *Server) connectAgent(ctx context.Context, agent *store.Agent, version string, session *yamux.Session) (*store.Environment, *store.Node, error) {
	env, node, err := s.store.UpsertAgentNode(ctx, agent.ID, agent.Name)
	if err != nil {
		return nil, nil, err
	}
	if err := s.store.TouchAgent(ctx, agent.ID, version); err != nil {
		return nil, nil, err
	}

	s.agents.add(agent.ID, session)
	if err := s.pool.RegisterAgent(env, node, tunnel.Dialer(session)); err != nil {
		s.agents.remove(agent.ID, session)
		return nil, nil, err
	}

	// Ask the daemon what it is, now that we can. A node that turns out to be part of a Swarm we
	// already know is attached to that Swarm's environment here — which is the whole of "membership
	// is discovered, never asserted": the agent never told us, and could not have.
	s.reconcileNode(ctx, env.ID, mustNode(s.pool, env.ID, node.ID))

	// The move may have re-homed it, so re-read where it actually ended up.
	if moved, err := s.store.NodeByID(ctx, node.ID); err == nil && moved.EnvID != env.ID {
		if e, err := s.store.EnvironmentByID(ctx, moved.EnvID); err == nil {
			return e, moved, nil
		}
	}
	return env, node, nil
}

// mustNode fetches a live node handle that was registered a moment ago. If the pool has already
// lost it, reconciliation simply has nothing to ask, and a nil node is handled by its caller.
func mustNode(pool *dockerx.Pool, envID, nodeID string) *dockerx.Node {
	env, err := pool.Get(envID)
	if err != nil {
		return nil
	}
	n, err := env.Node(nodeID)
	if err != nil {
		return nil
	}
	return n
}

func (s *Server) disconnectAgent(agentID, envID, nodeID string, session *yamux.Session) {
	s.agents.remove(agentID, session)

	// Only tear the node out of the pool if no NEWER session has taken over — otherwise a
	// reconnect races its own predecessor's cleanup and the agent ends up connected but unusable.
	s.agents.mu.RLock()
	_, stillConnected := s.agents.sessions[agentID]
	s.agents.mu.RUnlock()

	if !stillConnected {
		s.pool.Deregister(envID, nodeID)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// ── admin: managing agents ──────────────────────────────────────────────────────

// agentView is one agent row: its record, joined against whether it is connected right now.
type agentView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Status   string     `json:"status"`
	Enrolled bool       `json:"enrolled"`
	Version  string     `json:"version"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.agents.mu.RLock()
	defer s.agents.mu.RUnlock()

	out := make([]agentView, 0, len(agents))
	for _, a := range agents {
		v := agentView{ID: a.ID, Name: a.Name, Enrolled: a.Enrolled(), Version: a.Version}
		if !a.LastSeenAt.IsZero() {
			t := a.LastSeenAt
			v.LastSeen = &t
		}
		switch {
		case s.agents.sessions[a.ID] != nil:
			v.Status = "online"
		case !a.Enrolled():
			v.Status = "pending" // a join token was issued; the host has not checked in yet
		default:
			v.Status = "offline"
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type createAgentRequest struct {
	Name string `json:"name"`
}

// newAgentResponse is the enrolment answer. The join token appears here and never again —
// it exists only in the operator's clipboard.
type newAgentResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	JoinToken string `json:"join_token"`
	ExpiresIn int    `json:"expires_in"` // seconds until the token stops enrolling
}

// handleCreateAgent declares an agent and mints its one-time join token. The token is
// returned here and never again — it exists only in the operator's clipboard.
func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpx.BadRequest(w, r, "A name is required.")
		return
	}

	agent := &store.Agent{Name: req.Name}
	if u, ok := auth.UserFrom(r.Context()); ok {
		agent.CreatedBy = u.ID
	}
	if err := s.store.CreateAgent(r.Context(), agent); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken",
			"An agent with that name already exists.")
		return
	}

	token, err := randomToken()
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.store.CreateJoinToken(r.Context(), auth.HashToken(token), agent.ID, time.Now().Add(joinTokenTTL)); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "agent.create", Target: agent.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, newAgentResponse{
		ID:        agent.ID,
		Name:      agent.Name,
		JoinToken: token,
		ExpiresIn: int(joinTokenTTL.Seconds()),
	})
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	agent, err := s.store.AgentByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_agent", "No such agent.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Cut the tunnel first: a removed agent must not keep serving requests through a
	// session that outlives its own record.
	s.agents.mu.Lock()
	if sess, ok := s.agents.sessions[id]; ok {
		_ = sess.Close()
		delete(s.agents.sessions, id)
	}
	s.agents.mu.Unlock()

	// Drop the node from the pool before the row goes, so nothing is left dialing a tunnel that
	// is about to have no record. DeleteAgent removes the node — and the environment with it, if
	// that node was the last one in it.
	if node, err := s.store.NodeByAgent(r.Context(), id); err == nil {
		s.pool.Deregister(node.EnvID, node.ID)
	}
	if err := s.store.DeleteAgent(r.Context(), id); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "agent.delete", Target: agent.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}
