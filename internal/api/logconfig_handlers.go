package api

// Default container logging for deployed stacks: the fleet-wide default under
// /api/settings/logging and a per-host override under /api/clusters/{cluster}/logging.
// The config is injected into deploys in buildBundle; see docs/stacks.md.

import (
	"errors"
	"net/http"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// logConfigRequest is the two fields a person can actually choose. It is NOT
// store.LogConfig: that struct carries UpdatedAt, which is the store's to stamp — asking
// the client to send a field it is not allowed to set is how the monitor settings panel
// once broke on every save.
type logConfigRequest struct {
	Driver string            `json:"driver"`
	Opts   map[string]string `json:"opts"`
}

func decodeLogConfig(w http.ResponseWriter, r *http.Request) (*store.LogConfig, bool) {
	var body logConfigRequest
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return nil, false
	}
	return &store.LogConfig{Driver: body.Driver, Opts: body.Opts}, true
}

// saveLogConfigErr maps the store's refusal to the 400 it owes the person who typed the
// config; everything else stays a 500.
func saveLogConfigErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrInvalidLogConfig) {
		httpx.Fail(w, r, http.StatusBadRequest, "bad_log_config", err.Error())
		return
	}
	httpx.Error(w, r, err)
}

// ── the fleet default ─────────────────────────────────────────────────────────────

func (s *Server) handleGetGlobalLogConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GlobalLogConfig(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, cfg) // null when unset — a real state, not an error
}

func (s *Server) handleSaveGlobalLogConfig(w http.ResponseWriter, r *http.Request) {
	cfg, ok := decodeLogConfig(w, r)
	if !ok {
		return
	}
	if err := s.store.SaveGlobalLogConfig(r.Context(), cfg); err != nil {
		saveLogConfigErr(w, r, err)
		return
	}
	s.auditNotify(r, "logging.settings.update", "logging")
	httpx.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleDeleteGlobalLogConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteGlobalLogConfig(r.Context()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.auditNotify(r, "logging.settings.clear", "logging")
	w.WriteHeader(http.StatusNoContent)
}

// ── the per-host override ─────────────────────────────────────────────────────────

// envLogConfigResponse answers "what applies to MY host" in one shape: the override, the
// global default, and which of them is in effect. The global rides along because a
// host-scoped holder cannot call the scopeGlobal route — and a driver name and rotation
// numbers are not secrets.
type envLogConfigResponse struct {
	Override  *store.LogConfig `json:"override"`
	Global    *store.LogConfig `json:"global"`
	Effective *store.LogConfig `json:"effective"`
}

func (s *Server) handleGetEnvLogConfig(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("cluster")
	override, err := s.store.EnvLogConfig(r.Context(), envID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	global, err := s.store.GlobalLogConfig(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	effective := override
	if effective == nil {
		effective = global
	}
	httpx.JSON(w, http.StatusOK, envLogConfigResponse{
		Override:  override,
		Global:    global,
		Effective: effective,
	})
}

func (s *Server) handleSaveEnvLogConfig(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("cluster")
	// The FK would refuse a vanished host anyway, but as a 500. Say what happened.
	if _, err := s.store.EnvironmentByID(r.Context(), envID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	cfg, ok := decodeLogConfig(w, r)
	if !ok {
		return
	}
	if err := s.store.SaveEnvLogConfig(r.Context(), envID, cfg); err != nil {
		saveLogConfigErr(w, r, err)
		return
	}
	s.auditLogConfig(r, "logging.host.update", envID)
	httpx.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleDeleteEnvLogConfig(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("cluster")
	if err := s.store.DeleteEnvLogConfig(r.Context(), envID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.auditLogConfig(r, "logging.host.clear", envID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditLogConfig(r *http.Request, action, envID string) {
	u, _ := auth.UserFrom(r.Context())
	e := store.AuditEntry{EnvID: envID, Action: action, Target: envID, Outcome: "ok"}
	if u != nil {
		e.UserID, e.UserLabel = u.ID, u.Label()
	}
	s.audit(r.Context(), e)
}
