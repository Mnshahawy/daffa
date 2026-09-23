package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// ── the samples ─────────────────────────────────────────────────────────────────

// handleSeries serves a chart.
//
// Guarded by containers.view at the host's scope, which the route table already checked — a
// container's history is no more sensitive than the live stats panel that capability already
// grants, so it needs no capability of its own.
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	env := r.PathValue("cluster")

	to := time.Now().UTC()
	from := to.Add(-parseRange(r.URL.Query().Get("range")))

	pts, err := s.store.Series(r.Context(), store.SeriesQuery{
		EnvID:     env,
		Container: r.URL.Query().Get("container"),
		Stack:     r.URL.Query().Get("stack"),
		Host:      r.URL.Query().Get("host") == "true",
		Node:      r.URL.Query().Get("node"),
		From:      from,
		To:        to,
		MaxPoints: 240,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if pts == nil {
		pts = []store.Point{}
	}
	httpx.JSON(w, http.StatusOK, pts)
}

// parseRange turns ?range=24h into a duration, and anything it does not recognise into an
// hour. A chart is a read; there is nothing here worth a 400 over.
func parseRange(v string) time.Duration {
	switch v {
	case "6h":
		return 6 * time.Hour
	case "24h":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	default:
		return time.Hour
	}
}

// ── the settings ────────────────────────────────────────────────────────────────

// monitorConfigResponse carries the settings plus the numbers the form needs to say why
// a value was refused BEFORE the save: the server's floor and ceiling, and what the
// samples currently cost on disk.
type monitorConfigResponse struct {
	Settings     store.MonitorSettings `json:"settings"`
	Usage        store.MetricsUsage    `json:"usage"`
	MaxRetention int                   `json:"max_retention"`
	MinInterval  int                   `json:"min_interval"`
}

func (s *Server) handleGetMonitorSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.MonitorSettings(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	usage, err := s.store.Usage(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	httpx.JSON(w, http.StatusOK, monitorConfigResponse{
		Settings:     *cfg,
		Usage:        *usage,
		MaxRetention: store.MaxRetentionDays,
		MinInterval:  store.MinIntervalSecs,
	})
}

// monitorSettingsRequest is the three numbers a person can actually choose.
//
// It is NOT store.MonitorSettings, and that is the whole point: that struct carries an
// UpdatedAt, which is OURS — the store stamps it on write. Accepting it from the wire meant
// the client had to send one, and a Go time.Time cannot decode from the empty string the
// form sent, so EVERY save of this panel failed with "invalid JSON body" — including the
// valid ones. A field the client is not allowed to set is a field the client should not be
// asked to send.
type monitorSettingsRequest struct {
	Enabled       bool `json:"enabled"`
	IntervalSecs  int  `json:"interval_secs"`
	RetentionDays int  `json:"retention_days"`
}

func (s *Server) handleSaveMonitorSettings(w http.ResponseWriter, r *http.Request) {
	var body monitorSettingsRequest
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	req := store.MonitorSettings{
		Enabled:       body.Enabled,
		IntervalSecs:  body.IntervalSecs,
		RetentionDays: body.RetentionDays,
	}

	if err := s.store.SaveMonitorSettings(r.Context(), &req); err != nil {
		// Both of these are a person choosing a number we do not allow, not a server that broke.
		// A 500 here would tell them "something went wrong on our side" and swallow the sentence
		// that says what the allowed number is.
		if errors.Is(err, store.ErrRetentionTooLong) {
			httpx.Fail(w, r, http.StatusBadRequest, "retention_too_long", err.Error())
			return
		}
		if errors.Is(err, store.ErrIntervalTooShort) {
			httpx.Fail(w, r, http.StatusBadRequest, "interval_too_short", err.Error())
			return
		}
		httpx.Error(w, r, err)
		return
	}

	s.auditNotify(r, "monitoring.settings.update", "monitoring")
	httpx.JSON(w, http.StatusOK, req)
}

// ── the monitors ────────────────────────────────────────────────────────────────

func (s *Server) handleListMonitors(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.MonitorsView)
	list, err := s.store.ListMonitors(r.Context(), global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if list == nil {
		list = []*store.Monitor{}
	}
	httpx.JSON(w, http.StatusOK, list)
}

// handleCreateMonitor is the THIRD body-scoped route, and the check it makes for itself is the
// one no middleware can.
//
// The host a monitor watches arrives in the request BODY, so the route table cannot see it. And
// an EMPTY host means the monitor watches the whole fleet — which is a fleet-wide object, and
// therefore takes monitors.edit GLOBALLY. Without that rule, a contractor scoped to staging
// could create a monitor with no host filter and start receiving alerts about production
// containers, by name, which is precisely the leak scoping exists to prevent.
func (s *Server) handleCreateMonitor(w http.ResponseWriter, r *http.Request) {
	var m store.Monitor
	if err := httpx.Decode(w, r, &m); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	if !s.mayMonitor(w, r, m.EnvID) || !s.mayAct(w, r, &m) {
		return
	}
	if !s.sealActionSecrets(w, r, &m, nil) {
		return
	}

	if err := s.store.CreateMonitor(r.Context(), &m); err != nil {
		if errors.Is(err, store.ErrInvalidMonitor) {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_monitor", err.Error())
			return
		}
		httpx.Error(w, r, err)
		return
	}

	s.auditNotify(r, "monitor.create", m.Name)
	httpx.JSON(w, http.StatusCreated, scrubActions(&m))
}

// handleUpdateMonitor checks BOTH ends of the move.
//
// The route's scopeMonitor guard already proved the caller may act on the monitor as it stands.
// This checks where it is going: otherwise a staging-scoped holder could take their own staging
// monitor and re-point it at production, or clear its host filter entirely and have it watch
// the fleet — a privilege escalation by edit, which is the one an "is this yours?" check on the
// way in cannot catch.
func (s *Server) handleUpdateMonitor(w http.ResponseWriter, r *http.Request) {
	var m store.Monitor
	if err := httpx.Decode(w, r, &m); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	m.ID = r.PathValue("id")

	if !s.mayMonitor(w, r, m.EnvID) || !s.mayAct(w, r, &m) {
		return
	}
	prev, err := s.store.MonitorByID(r.Context(), m.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !s.sealActionSecrets(w, r, &m, prev) {
		return
	}

	if err := s.store.UpdateMonitor(r.Context(), &m); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.Fail(w, r, http.StatusNotFound, "no_such_monitor", "No such monitor.")
			return
		}
		if errors.Is(err, store.ErrInvalidMonitor) {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_monitor", err.Error())
			return
		}
		httpx.Error(w, r, err)
		return
	}

	s.auditNotify(r, "monitor.update", m.Name)
	httpx.JSON(w, http.StatusOK, scrubActions(&m))
}

func (s *Server) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	m, err := s.store.MonitorByID(r.Context(), id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.store.DeleteMonitor(r.Context(), id); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.auditNotify(r, "monitor.delete", m.Name)
	w.WriteHeader(http.StatusNoContent)
}

// mayMonitor decides whether the caller may own a monitor that watches envID.
//
// An empty envID is a FLEET-WIDE monitor and needs the capability globally. A named one needs
// it on that host. This is the whole of the rule, and it is written once so that create and
// update cannot disagree about it.
func (s *Server) mayMonitor(w http.ResponseWriter, r *http.Request, envID string) bool {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return false
	}

	if envID == "" {
		if !u.Caps.Global.Has(caps.MonitorsEdit) {
			s.recordDenial(r, u, "missing_capability:monitors.edit(global)")
			httpx.Fail(w, r, http.StatusForbidden, "monitor_needs_a_host",
				"A monitor that watches every host is a fleet-wide rule, so it needs "+
					"monitors.edit everywhere. Pick a host for this monitor, or ask an "+
					"administrator.")
			return false
		}
		return true
	}

	return s.mayUseEnv(w, r, caps.MonitorsEdit, envID)
}

// actionCap is the capability each action wields — the one its hand-driven equivalent needs.
// Creating a monitor that restarts things is restarting things, later, unattended; it must not
// be a way around containers.edit. HTTP is settings.edit, fleet-wide: it makes the SERVER call
// an arbitrary URL, the same power as a notification channel, and inside a private network
// that is a request to anything the server can reach.
func actionCap(kind string) (c caps.Cap, global bool) {
	switch kind {
	case store.ActionRestart:
		return caps.ContainersEdit, false
	case store.ActionExec:
		return caps.ContainersExec, false
	case store.ActionScale:
		return caps.ServicesEdit, false
	case store.ActionHTTP:
		return caps.SettingsEdit, true
	}
	return caps.Cap{}, false // unknown kinds fail validation; the zero Cap fails closed
}

// mayAct checks the caller holds, on the monitor's cluster, every power its actions wield.
// Checked on every save, not only when actions change: editing a monitor re-arms what it will
// do, and a threshold change can be what makes an exec start firing.
func (s *Server) mayAct(w http.ResponseWriter, r *http.Request, m *store.Monitor) bool {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return false
	}
	kind, c, global, ok := missingActionCap(u, m)
	if ok {
		return true
	}
	s.recordDenial(r, u, "missing_capability:"+c.Name())
	where := " on this cluster"
	if global {
		where = " fleet-wide"
	}
	httpx.Fail(w, r, http.StatusForbidden, "missing_capability:"+c.Name(),
		fmt.Sprintf("A %s action needs %s%s — the power it would use, unattended.", kind, c.Name(), where))
	return false
}

// missingActionCap is mayAct's decision: the first action whose power u lacks, or ok.
func missingActionCap(u *store.User, m *store.Monitor) (kind string, c caps.Cap, global, ok bool) {
	for _, a := range m.Actions {
		c, global := actionCap(a.Kind)
		if c == (caps.Cap{}) {
			continue // validation names the unknown kind with a better sentence
		}
		held := u.Caps.Has(c, m.EnvID)
		if global {
			held = u.Caps.Global.Has(c)
		}
		if !held {
			return a.Kind, c, global, false
		}
	}
	return "", caps.Cap{}, false, true
}

// sealActionSecrets turns a plaintext signing secret into a sealed one. An HTTP action saved
// without a new secret keeps the one it had — the form cannot show it, so it cannot send it
// back — found by its URL, so reordering the actions keeps it too. By URL and never by
// position: carrying a secret to a new URL is sending it somewhere its owner never chose.
func (s *Server) sealActionSecrets(w http.ResponseWriter, r *http.Request, m *store.Monitor, prev *store.Monitor) bool {
	for i := range m.Actions {
		a := &m.Actions[i]
		a.SecretEnc = ""
		if a.Kind != store.ActionHTTP {
			a.Secret, a.HasSecret = "", false
			continue
		}
		switch {
		case a.Secret != "":
			sealed, err := s.sealer.Seal(a.Secret)
			if err != nil {
				httpx.Error(w, r, err)
				return false
			}
			a.SecretEnc = sealed
		case a.HasSecret && prev != nil:
			for _, p := range prev.Actions {
				if p.Kind == store.ActionHTTP && p.URL == strings.TrimSpace(a.URL) && p.SecretEnc != "" {
					a.SecretEnc = p.SecretEnc
					break
				}
			}
		}
		a.Secret = ""
	}
	return true
}

// scrubActions is the monitor as it may leave the server: no secret, sealed or not.
func scrubActions(m *store.Monitor) *store.Monitor {
	for i := range m.Actions {
		m.Actions[i].HasSecret = m.Actions[i].SecretEnc != ""
		m.Actions[i].Secret = ""
	}
	if m.Actions == nil {
		m.Actions = []store.MonitorAction{}
	}
	return m
}

// ── the alerts ──────────────────────────────────────────────────────────────────

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.MonitorsView)

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	list, err := s.store.ListAlerts(r.Context(), global, envs, limit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if list == nil {
		list = []*store.Alert{}
	}
	httpx.JSON(w, http.StatusOK, list)
}
