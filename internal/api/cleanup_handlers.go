package api

// Automatic disk cleanup: the fleet-wide policy under /api/settings/cleanup and the
// per-host override under /api/clusters/{cluster}/cleanup. Shaped like the logging
// defaults, and for the same reason — one default, overridable per host.
//
// Every route here is system.prune, not a capability of its own: scheduling a prune is
// the same power as pressing the button, aimed at 03:30 instead of now.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/robfig/cron/v3"

	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// cleanupPolicyRequest is the four things a person chooses. Not store.CleanupPolicy: that
// carries UpdatedAt, which is the store's to stamp.
type cleanupPolicyRequest struct {
	Enabled   bool     `json:"enabled"`
	Schedule  string   `json:"schedule"`
	Targets   []string `json:"targets"`
	KeepHours int      `json:"keep_hours"`
}

func decodeCleanupPolicy(w http.ResponseWriter, r *http.Request) (*store.CleanupPolicy, bool) {
	var body cleanupPolicyRequest
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return nil, false
	}

	targets := make([]store.CleanupTarget, 0, len(body.Targets))
	for _, t := range body.Targets {
		targets = append(targets, store.CleanupTarget(strings.TrimSpace(t)))
	}
	p := &store.CleanupPolicy{
		Enabled:   body.Enabled,
		Schedule:  strings.TrimSpace(body.Schedule),
		Targets:   targets,
		KeepHours: body.KeepHours,
	}

	// The cron is parsed here, with the library the scheduler uses, rather than discovered
	// to be nonsense at the hour it was meant to run. Same check as a backup job's schedule.
	if p.Schedule != "" {
		if _, err := cron.ParseStandard(p.Schedule); err != nil {
			httpx.BadRequest(w, r, "That is not a valid cron expression (e.g. \"30 3 * * *\" for 03:30 UTC daily).")
			return nil, false
		}
	}
	return p, true
}

// saveCleanupErr maps the store's refusal to the 400 it owes the person who typed the
// policy; everything else stays a 500.
func saveCleanupErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrInvalidCleanupPolicy) {
		httpx.Fail(w, r, http.StatusBadRequest, "bad_cleanup_policy", err.Error())
		return
	}
	httpx.Error(w, r, err)
}

// ── the fleet default ─────────────────────────────────────────────────────────────

func (s *Server) handleGetGlobalCleanup(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GlobalCleanupPolicy(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p) // null when unset — a real state, not an error
}

func (s *Server) handleSaveGlobalCleanup(w http.ResponseWriter, r *http.Request) {
	p, ok := decodeCleanupPolicy(w, r)
	if !ok {
		return
	}
	if err := s.store.SaveGlobalCleanupPolicy(r.Context(), p); err != nil {
		saveCleanupErr(w, r, err)
		return
	}
	// Live now, not at the next restart: a schedule that only takes effect after a
	// redeploy is a schedule that silently did nothing.
	s.rebuildCleanupSchedule(r.Context())
	s.auditNotify(r, "cleanup.settings.update", "cleanup")
	httpx.JSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteGlobalCleanup(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteGlobalCleanupPolicy(r.Context()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.rebuildCleanupSchedule(r.Context())
	s.auditNotify(r, "cleanup.settings.clear", "cleanup")
	w.WriteHeader(http.StatusNoContent)
}

// ── the per-host override ─────────────────────────────────────────────────────────

// envCleanupResponse answers "what happens to MY host, and did it happen" in one shape.
// The fleet default rides along because a host-scoped holder cannot call the global route
// and would otherwise see "no override" with no idea what that inherits.
type envCleanupResponse struct {
	Override  *store.CleanupPolicy `json:"override"`
	Global    *store.CleanupPolicy `json:"global"`
	Effective *store.CleanupPolicy `json:"effective"`
	LastRun   *store.CleanupRun    `json:"last_run,omitempty"`
}

func (s *Server) handleGetEnvCleanup(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("cluster")

	override, err := s.store.EnvCleanupPolicy(r.Context(), envID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	global, err := s.store.GlobalCleanupPolicy(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	last, err := s.store.LastCleanupRun(r.Context(), envID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	effective := override
	if effective == nil {
		effective = global
	}
	httpx.JSON(w, http.StatusOK, envCleanupResponse{
		Override: override, Global: global, Effective: effective, LastRun: last,
	})
}

func (s *Server) handleSaveEnvCleanup(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("cluster")
	// The FK would refuse a vanished host anyway, but as a 500. Say what happened.
	if _, err := s.store.EnvironmentByID(r.Context(), envID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	p, ok := decodeCleanupPolicy(w, r)
	if !ok {
		return
	}
	if err := s.store.SaveEnvCleanupPolicy(r.Context(), envID, p); err != nil {
		saveCleanupErr(w, r, err)
		return
	}
	s.rebuildCleanupSchedule(r.Context())
	s.auditCleanup(r, "cleanup.host.update", envID)
	httpx.JSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteEnvCleanup(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("cluster")
	if err := s.store.DeleteEnvCleanupPolicy(r.Context(), envID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.rebuildCleanupSchedule(r.Context())
	s.auditCleanup(r, "cleanup.host.clear", envID)
	w.WriteHeader(http.StatusNoContent)
}

// handleRunCleanup sweeps now, without waiting for the schedule. It answers 202 and runs
// in the background: pruning a few thousand images takes minutes, and an HTTP request is
// not a place to wait for one.
//
// It runs the host's EFFECTIVE policy — the same targets and the same age floor the cron
// would use — so "Run now" is a rehearsal of the nightly sweep rather than a second,
// differently-behaved button. A policy that is switched off still runs on demand: the
// person pressing it is asking for this sweep, now.
func (s *Server) handleRunCleanup(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("cluster")

	policy, err := s.store.EffectiveCleanupPolicy(r.Context(), envID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if policy == nil || len(policy.Targets) == 0 {
		httpx.Fail(w, r, http.StatusBadRequest, "no_cleanup_policy",
			"This host has no cleanup policy yet, so there is nothing to run. Choose what to prune first.")
		return
	}

	// Detached, with the same bound the scheduled sweep gets: this handler returns
	// immediately, which cancels r.Context().
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), cleanupRunTimeout)
		defer cancel()
		s.runCleanup(ctx, envID, "manual")
	}()

	httpx.JSON(w, http.StatusAccepted, statusResponse{Status: "started"})
}

func (s *Server) auditCleanup(r *http.Request, action, envID string) {
	s.audit(r.Context(), store.AuditEntry{
		EnvID: envID, Action: action, Target: envID, Outcome: "ok",
	})
}
