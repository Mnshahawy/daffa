package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMonitorActionValidation(t *testing.T) {
	base := func(actions ...MonitorAction) *Monitor {
		return &Monitor{Name: "leak", Metric: "mem_pct", Op: ">", Threshold: 90, DurationSecs: 600,
			EnvID: "env_prod", Actions: actions}
	}
	bad := map[string]*Monitor{
		"fleet-wide with actions": func() *Monitor { m := base(MonitorAction{Kind: ActionRestart}); m.EnvID = ""; return m }(),
		"unknown kind":            base(MonitorAction{Kind: "reboot"}),
		"exec without a command":  base(MonitorAction{Kind: ActionExec}),
		"exec waiting too long":   base(MonitorAction{Kind: ActionExec, Command: []string{"jcmd"}, TimeoutSecs: 3600}),
		"scale by nothing":        base(MonitorAction{Kind: ActionScale, Limit: 3}),
		"scale without a limit":   base(MonitorAction{Kind: ActionScale, By: 1}),
		"scale in to zero":        base(MonitorAction{Kind: ActionScale, By: -1, Limit: 0}),
		"http without a scheme":   base(MonitorAction{Kind: ActionHTTP, URL: "runbooks.lan/hook"}),
		"a cooldown of seconds":   func() *Monitor { m := base(MonitorAction{Kind: ActionRestart}); m.ActionCooldownSecs = 5; return m }(),
		"too many actions": base(MonitorAction{Kind: ActionRestart}, MonitorAction{Kind: ActionRestart},
			MonitorAction{Kind: ActionRestart}, MonitorAction{Kind: ActionRestart},
			MonitorAction{Kind: ActionRestart}, MonitorAction{Kind: ActionRestart}),
	}
	for name, m := range bad {
		if err := m.validate(); !errors.Is(err, ErrInvalidMonitor) {
			t.Errorf("%s: %v, want ErrInvalidMonitor", name, err)
		}
	}

	good := base(
		MonitorAction{Kind: ActionExec, Command: []string{"jcmd", "1", "GC.heap_dump", "/dumps/h.hprof"}},
		MonitorAction{Kind: ActionRestart},
	)
	if err := good.validate(); err != nil {
		t.Fatalf("dump-then-restart refused: %v", err)
	}
	if good.ActionCooldownSecs != DefaultActionCooldown {
		t.Errorf("an unset cooldown became %d, want the default %d", good.ActionCooldownSecs, DefaultActionCooldown)
	}
}

// Actions round-trip in order, the sealed secret survives the store without ever being a
// field the wire can carry, and the cooldown's clock is keyed on the target across alerts.
func TestMonitorActionsRoundTripAndCooldownClock(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)
		m := &Monitor{Name: "leak", Enabled: true, Metric: "mem_pct", Op: ">", Threshold: 90,
			DurationSecs: 600, EnvID: env, Actions: []MonitorAction{
				{Kind: ActionExec, Command: []string{"sh", "-c", "kill -3 1"}},
				{Kind: ActionHTTP, URL: "https://runbooks.lan/hook", SecretEnc: "sealed"},
				{Kind: ActionRestart},
			}}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}
		got, err := s.MonitorByID(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Actions) != 3 || got.Actions[0].Kind != ActionExec || got.Actions[2].Kind != ActionRestart {
			t.Fatalf("actions after a round trip: %+v", got.Actions)
		}
		if h := got.Actions[1]; h.SecretEnc != "sealed" || !h.HasSecret || h.Secret != "" {
			t.Errorf("http action secret after a round trip: %+v", h)
		}
		if got.ActionCooldownSecs != DefaultActionCooldown {
			t.Errorf("cooldown = %d", got.ActionCooldownSecs)
		}

		if last, err := s.LastActionAt(ctx, m.ID, "api"); err != nil || !last.IsZero() {
			t.Fatalf("a target that never acted has a last action: %v %v", last, err)
		}
		first := &Alert{MonitorID: m.ID, EnvID: env, ContainerName: "api"}
		if err := s.RaiseAlert(ctx, first); err != nil {
			t.Fatal(err)
		}
		if err := s.ClaimAlertActions(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordAlertActions(ctx, first.ID, []ActionResult{{Kind: ActionRestart, OK: true, Detail: "Restarted api-1."}}); err != nil {
			t.Fatal(err)
		}
		// Resolved and fired again: a new alert, but the SAME target's clock.
		_ = s.ResolveAlert(ctx, first.ID, "back within the threshold")
		second := &Alert{MonitorID: m.ID, EnvID: env, ContainerName: "api"}
		if err := s.RaiseAlert(ctx, second); err != nil {
			t.Fatal(err)
		}
		if last, _ := s.LastActionAt(ctx, m.ID, "api"); time.Since(last) > time.Minute {
			t.Errorf("the refire does not see the first alert's actions: last = %v", last)
		}

		alerts, err := s.ListAlerts(ctx, true, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range alerts {
			if a.ID == first.ID && (len(a.Actions) != 1 || !a.Actions[0].OK) {
				t.Errorf("the recorded actions did not come back on the alert: %+v", a.Actions)
			}
		}
	})
}

// The machine's own samples are not a container. Before this, a rule with no container filter
// judged the whole host as a target called ":host" — and with actions would have tried to
// restart it.
func TestTheHostIsNotATarget(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)
		m := &Monitor{Name: "any", Enabled: true, Metric: "mem_pct", Op: ">", Threshold: 50,
			DurationSecs: 600, EnvID: env}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Second)
		writeSeries(t, s, env, "api", at, 30*time.Second, 20, func(int) float64 { return 80 })
		writeSeries(t, s, env, HostSentinel, at, 30*time.Second, 20, func(int) float64 { return 80 })
		got, err := s.Evaluate(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range got {
			if b.ContainerName == HostSentinel {
				t.Errorf("the host was evaluated as a container: %+v", b)
			}
		}
		if len(got) != 1 {
			t.Errorf("evaluated to %d targets, want just api", len(got))
		}
	})
}

// 0022 on a populated 0021 database: existing monitors keep alerting, with no actions and the
// default cooldown, and existing alerts read back with an empty action log.
func TestMigrate0022MonitorActions(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(t.TempDir(), "test.db")
	stopAfter = "0021_webhooks"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0021: %v", err)
	}
	defer s.Close()
	env := &Environment{Name: "prod"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO resource_monitors (id, name, enabled, metric, op, threshold, duration_secs, env_id, created_at, updated_at)
		 VALUES ('mon_old', 'old', 1, 'mem_pct', '>', 90, 600, '` + env.ID + `', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO monitor_alerts (id, monitor_id, env_id, container_name, state, started_at, last_seen_at)
		 VALUES ('alr_old', 'mon_old', '` + env.ID + `', 'api', 'firing', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrating to 0022: %v", err)
	}
	m, err := s.MonitorByID(ctx, "mon_old")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Actions) != 0 || m.ActionCooldownSecs != DefaultActionCooldown || !m.Enabled {
		t.Errorf("monitor after 0022: %+v", m)
	}
	alerts, err := s.ListAlerts(ctx, true, nil, 10)
	if err != nil || len(alerts) != 1 || alerts[0].Actions != nil {
		t.Errorf("alerts after 0022: %+v, %v", alerts, err)
	}
}
