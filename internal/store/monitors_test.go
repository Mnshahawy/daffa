package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The rule fires only when the condition held for the WHOLE window.
//
// "Memory above 70% for more than ten minutes" means what it says: one sample below the line
// resets the clock. This is Prometheus's `for:` semantic, and it is the one that agrees with
// the graph — which matters, because the first thing anybody does when an alert fires is open
// the graph, and an alert the graph does not support is an alert nobody trusts again.
func TestSustainedMeansEverySample(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "Memory high", Enabled: true, Metric: "mem_pct", Op: ">",
			Threshold: 70, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		at := time.Now().UTC().Truncate(time.Second)
		const interval = 30 * time.Second
		expected := 20 // ten minutes of thirty-second samples

		// Twenty samples, every one of them over the line.
		writeSeries(t, s, env, "api", at, interval, 20, func(int) float64 { return 75 })

		got, err := s.Evaluate(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("evaluated to %d rows; want 1", len(got))
		}
		if !got[0].Sustained(expected) {
			t.Errorf("twenty samples all above 70%% did not sustain: %d of %d breaching",
				got[0].Breaching, got[0].Samples)
		}

		// Now the same window with ONE dip in the middle. The clock resets.
		s2 := freshHost(t, s)
		writeSeries(t, s2.store, s2.env, "api", at, interval, 20, func(i int) float64 {
			if i == 9 {
				return 68 // one reading below the line, for thirty seconds
			}
			return 75
		})
		m.EnvID = s2.env
		if err := s.UpdateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		got, err = s.Evaluate(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("evaluated to %d rows; want 1", len(got))
		}
		if got[0].Sustained(expected) {
			t.Errorf("a window with one sample below the threshold still sustained "+
				"(%d of %d breaching). One dip must reset the clock, or 'for ten minutes' "+
				"does not mean for ten minutes.", got[0].Breaching, got[0].Samples)
		}
	})
}

// The coverage floor, and why it is not decoration.
//
// A host that was offline for the whole window leaves ONE stale sample behind. Without a floor,
// that single reading satisfies "every sample in the window breached" — with a sample size of
// one — and pages somebody about a machine that has not spoken in an hour. Which is both wrong
// and redundant: agent.offline already told them.
func TestOneStaleSampleIsNotTenMinutesOfEvidence(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "Memory high", Enabled: true, Metric: "mem_pct", Op: ">",
			Threshold: 70, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		at := time.Now().UTC().Truncate(time.Second)

		// Exactly one sample in the whole ten-minute window, and it is over the line.
		if err := s.InsertSamples(ctx, []Sample{{
			TS: at.Add(-9 * time.Minute), EnvID: env, ContainerName: "api", MemPct: 99,
		}}); err != nil {
			t.Fatal(err)
		}

		got, err := s.Evaluate(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("evaluated to %d rows; want 1", len(got))
		}
		if got[0].Breaching != 1 || got[0].Samples != 1 {
			t.Fatalf("expected a single breaching sample, got %d of %d", got[0].Breaching, got[0].Samples)
		}

		// One sample out of an expected twenty is not ten minutes of anything.
		if got[0].Sustained(20) {
			t.Error("a single stale sample was accepted as ten minutes of sustained breach — " +
				"the coverage floor is not doing its job, and a host that has been offline " +
				"for an hour is about to page somebody")
		}
	})
}

// A container that has only just started cannot have been over the line for ten minutes, and
// must not be treated as though it had.
func TestANewContainerCannotAlreadyBeSustained(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "CPU high", Enabled: true, Metric: "cpu_pct", Op: ">",
			Threshold: 70, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		at := time.Now().UTC().Truncate(time.Second)

		// It has been up for two minutes, and it is pegged. That is four samples, not twenty.
		writeSeries(t, s, env, "api", at, 30*time.Second, 4, func(int) float64 { return 99 })

		got, err := s.Evaluate(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Sustained(20) {
			t.Error("a container two minutes old satisfied a ten-minute rule")
		}
	})
}

// A swarm service is watched as a service, not as its task containers — because those come and go.
//
// A rolling update, a reschedule, or a scale event replaces a replica with a new task whose
// container name carries a fresh id. Keyed on the container, the sustained window would reset the
// instant that happened: ten minutes of a pegged service, split across two task names, would read
// as two five-minute runs and never fire. The service name is the thing that stays.
func TestAServiceOutlivesItsTaskContainers(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "CPU high", Enabled: true, Metric: "cpu_pct", Op: ">",
			Threshold: 70, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		at := time.Now().UTC().Truncate(time.Second)
		const interval = 30 * time.Second

		// Twenty rounds, every one pegged — but a rolling update swaps the task container halfway,
		// so the first ten samples belong to one ephemeral container name and the last ten to
		// another. Both are the same service, `web`.
		for i := range 20 {
			ts := at.Add(-time.Duration(19-i) * interval)
			name := "web.1.aaaaaaaaaaaa"
			if i >= 10 {
				name = "web.1.bbbbbbbbbbbb"
			}
			if err := s.InsertSamples(ctx, []Sample{{
				TS: ts, EnvID: env, ContainerID: "x", ContainerName: name, Service: "web", CPUPct: 99,
			}}); err != nil {
				t.Fatal(err)
			}
		}

		got, err := s.Evaluate(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("evaluated to %d rows; want 1 — the service, not one per task container", len(got))
		}
		if got[0].ContainerName != "web" {
			t.Errorf("target = %q; want the service name %q", got[0].ContainerName, "web")
		}
		if !got[0].Sustained(20) {
			t.Errorf("a service pegged for the whole window did not sustain across a task swap "+
				"(%d of %d) — keyed on the ephemeral container it never could", got[0].Breaching, got[0].Samples)
		}
	})
}

// The service is breaching whenever ANY of its replicas is — and the replicas are collapsed to one
// value per timestamp first, so coverage is counted in timestamps, not in replica-rows.
func TestAServiceBreachesWhenAnyReplicaDoes(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "CPU high", Enabled: true, Metric: "cpu_pct", Op: ">",
			Threshold: 70, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		at := time.Now().UTC().Truncate(time.Second)
		const interval = 30 * time.Second

		// Two replicas at every one of the twenty timestamps: one pegged, one idle.
		for i := range 20 {
			ts := at.Add(-time.Duration(19-i) * interval)
			if err := s.InsertSamples(ctx, []Sample{
				{TS: ts, EnvID: env, ContainerID: "hot", ContainerName: "web.1.aaa", Service: "web", CPUPct: 95},
				{TS: ts, EnvID: env, ContainerID: "cold", ContainerName: "web.2.bbb", Service: "web", CPUPct: 5},
			}); err != nil {
				t.Fatal(err)
			}
		}

		got, err := s.Evaluate(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("evaluated to %d rows; want 1 service target", len(got))
		}
		// Twenty timestamps, not forty replica-rows — else two replicas would satisfy a ten-minute
		// coverage floor in five minutes.
		if got[0].Samples != 20 {
			t.Errorf("samples = %d; want 20 (per timestamp, replicas collapsed)", got[0].Samples)
		}
		if !got[0].Sustained(20) {
			t.Errorf("a service with a pegged replica at every timestamp did not sustain (%d of %d)",
				got[0].Breaching, got[0].Samples)
		}
		// The reported value is the worst replica's, not the idle one's.
		if got[0].Worst < 90 {
			t.Errorf("worst = %v; want the hot replica's ~95, not the idle 5", got[0].Worst)
		}
	})
}

// '<' is the "is this thing dead?" alert, and the value it reports must be the LOW one. Telling
// somebody the maximum CPU of a worker you are alerting on for being idle is telling them the
// number they are least interested in.
func TestTheReportedValueIsTheOneTheRuleCaresAbout(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "Worker idle", Enabled: true, Metric: "cpu_pct", Op: "<",
			Threshold: 5, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		at := time.Now().UTC().Truncate(time.Second)
		writeSeries(t, s, env, "worker", at, 30*time.Second, 20, func(i int) float64 {
			if i == 5 {
				return 0.5
			}
			return 2
		})

		got, err := s.Evaluate(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		if !got[0].Sustained(20) {
			t.Fatal("a worker idling below 5% for ten minutes did not sustain a '<' rule")
		}
		if got[0].Worst > 1 {
			t.Errorf("a '<' rule reported %.1f — the extreme in the direction it cares about "+
				"is the LOWEST reading (0.5), not the highest", got[0].Worst)
		}
	})
}

// The alert state machine: raise once, keep the value current, resolve, and be able to fire
// again afterwards.
func TestAnAlertFiresOnceAndCanFireAgain(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "Memory high", Enabled: true, Metric: "mem_pct", Op: ">",
			Threshold: 70, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		a := &Alert{MonitorID: m.ID, EnvID: env, ContainerName: "api", Value: 82}
		if err := s.RaiseAlert(ctx, a); err != nil {
			t.Fatal(err)
		}

		// While it is firing, it is found — which is what stops the evaluator raising a second
		// one, and mailing about it, every thirty seconds.
		open, err := s.FiringAlert(ctx, m.ID, "api")
		if err != nil {
			t.Fatalf("a firing alert was not found: %v", err)
		}
		if open.ID != a.ID {
			t.Errorf("found the wrong alert")
		}

		if err := s.ResolveAlert(ctx, a.ID, "back under"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FiringAlert(ctx, m.ID, "api"); err == nil {
			t.Error("a resolved alert is still reported as firing")
		}

		// And it can fire again later — the same container, the same monitor, a new incident.
		if err := s.RaiseAlert(ctx, &Alert{
			MonitorID: m.ID, EnvID: env, ContainerName: "api", Value: 91,
		}); err != nil {
			t.Fatalf("the same container could not raise a second alert later: %v", err)
		}

		// Both are on the record. "It was in trouble for an hour last night and recovered" is
		// the thing you most want to find in the morning.
		all, err := s.ListAlerts(ctx, true, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 {
			t.Errorf("got %d alerts; want 2 (one resolved, one firing)", len(all))
		}
		// Firing sorts first.
		if all[0].State != AlertFiring {
			t.Errorf("the firing alert is not at the top of the list")
		}
	})
}

// Disabling a monitor must not leave its alerts firing forever.
func TestDisablingAMonitorResolvesItsAlerts(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "Memory high", Enabled: true, Metric: "mem_pct", Op: ">",
			Threshold: 70, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err := s.RaiseAlert(ctx, &Alert{
			MonitorID: m.ID, EnvID: env, ContainerName: "api", Value: 82,
		}); err != nil {
			t.Fatal(err)
		}

		m.Enabled = false
		if err := s.UpdateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		if _, err := s.FiringAlert(ctx, m.ID, "api"); err == nil {
			t.Error("disabling a monitor left its alert firing — it would sit in the UI as a " +
				"live incident forever, on a rule nobody is evaluating")
		}
	})
}

// A stale alert is one whose container stopped reporting: it was redeployed, removed, or its
// host went away. An alert that hangs firing forever on a container that no longer exists is
// the one that teaches everybody to ignore the page.
func TestStaleAlertsAreFound(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		m := &Monitor{
			Name: "Memory high", Enabled: true, Metric: "mem_pct", Op: ">",
			Threshold: 70, DurationSecs: 600, EnvID: env,
		}
		if err := s.CreateMonitor(ctx, m); err != nil {
			t.Fatal(err)
		}

		// Two alerts: one touched just now, one last seen five minutes ago.
		fresh := &Alert{MonitorID: m.ID, EnvID: env, ContainerName: "alive", Value: 80}
		gone := &Alert{MonitorID: m.ID, EnvID: env, ContainerName: "removed", Value: 80}
		if err := s.RaiseAlert(ctx, fresh); err != nil {
			t.Fatal(err)
		}

		// Freeze the clock to age the second one, the way the store's other tests do.
		real := now
		now = func() time.Time { return real().Add(-5 * time.Minute) }
		err := s.RaiseAlert(ctx, gone)
		now = real
		if err != nil {
			t.Fatal(err)
		}

		stale, err := s.StaleAlerts(ctx, time.Now().Add(-90*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if len(stale) != 1 || stale[0].ContainerName != "removed" {
			t.Fatalf("stale alerts = %v; want just the container that stopped reporting", names(stale))
		}
	})
}

// Lists filter, they do not gate. A host-scoped holder sees their host's monitors — and NOT the
// fleet-wide ones, which watch hosts they have no standing on.
func TestMonitorListsAreFiltered(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, staging := twoHosts(t, s)

		for _, m := range []*Monitor{
			{Name: "prod memory", Enabled: true, Metric: "mem_pct", Op: ">", Threshold: 70, DurationSecs: 600, EnvID: prod.ID},
			{Name: "staging memory", Enabled: true, Metric: "mem_pct", Op: ">", Threshold: 70, DurationSecs: 600, EnvID: staging.ID},
			{Name: "everywhere", Enabled: true, Metric: "cpu_pct", Op: ">", Threshold: 90, DurationSecs: 600},
		} {
			if err := s.CreateMonitor(ctx, m); err != nil {
				t.Fatal(err)
			}
		}

		// A staging-scoped holder.
		got, err := s.ListMonitors(ctx, false, []string{staging.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "staging memory" {
			t.Errorf("a staging-scoped holder sees %v; want only the staging monitor. The "+
				"fleet-wide one watches production containers, and its alerts name them.",
				monitorNames(got))
		}

		// An administrator sees all three, fleet-wide rule included.
		got, err = s.ListMonitors(ctx, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Errorf("an administrator sees %d monitors; want 3", len(got))
		}

		// And somebody holding the capability nowhere sees nothing — never everything, which is
		// what an empty IN () clause would otherwise quietly become.
		got, err = s.ListMonitors(ctx, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("a holder with no hosts saw %d monitors", len(got))
		}
	})
}

// A rule's metric and comparison end up in a SQL fragment, so they are resolved through a map
// and never interpolated. This is the fence.
func TestAMonitorCannotCarryArbitrarySQL(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		bad := []*Monitor{
			{Name: "x", Metric: "mem_pct; DROP TABLE users", Op: ">", Threshold: 1, DurationSecs: 600},
			{Name: "x", Metric: "mem_pct", Op: "> 0 OR 1=1 --", Threshold: 1, DurationSecs: 600},
			{Name: "x", Metric: "1", Op: ">", Threshold: 1, DurationSecs: 600},
		}
		for _, m := range bad {
			if err := s.CreateMonitor(ctx, m); err == nil {
				t.Errorf("a monitor with metric=%q op=%q was accepted", m.Metric, m.Op)
			}
		}

		// And the ordinary validation, which is about people rather than attackers.
		if err := s.CreateMonitor(ctx, &Monitor{
			Name: "x", Metric: "mem_pct", Op: ">", Threshold: 70, DurationSecs: 5,
		}); err == nil {
			t.Error("a five-second duration was accepted — that fires on a single sample, and " +
				"every passing spike becomes a page")
		}
		if err := s.CreateMonitor(ctx, &Monitor{
			Name: "x", Metric: "mem_pct", Op: ">", Threshold: 300, DurationSecs: 600,
		}); err == nil {
			t.Error("a threshold of 300% was accepted for a percentage metric — it can never fire")
		}
	})
}

// The absolute metrics exist for the container nobody gave a limit to, and this is that
// container: unlimited, on a sixteen-core box, chewing four cores flat.
//
// Its allowance is the whole machine, so it is at 25% of what it is allowed — and a percentage
// rule, however it is written, is measuring against a ceiling of sixteen cores. `cpu > 80%` will
// not fire for this container this side of the heat death of the universe. `cpu > 2 vCPU` fires,
// which is the entire point.
func TestAnAbsoluteRuleCatchesWhatAPercentageRuleCannot(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		at := time.Now().UTC().Truncate(time.Second)
		for i := range 20 {
			ts := at.Add(-time.Duration(19-i) * 30 * time.Second)
			if err := s.InsertSamples(ctx, []Sample{{
				TS: ts, EnvID: env, ContainerID: "abc", ContainerName: "api",
				CPUPct:   25, // a quarter of its allowance...
				CPUCores: 16, // ...which is the whole host, because it has no limit
				MemBytes: 4e9, MemLimit: 64e9, MemPct: 6.25,
			}}); err != nil {
				t.Fatal(err)
			}
		}

		pct := &Monitor{
			Name: "CPU high", Enabled: true, Metric: "cpu_pct", Op: ">",
			Threshold: 80, DurationSecs: 600, EnvID: env,
		}
		cores := &Monitor{
			Name: "CPU cores high", Enabled: true, Metric: "cpu_cores", Op: ">",
			Threshold: 2, DurationSecs: 600, EnvID: env,
		}
		for _, m := range []*Monitor{pct, cores} {
			if err := s.CreateMonitor(ctx, m); err != nil {
				t.Fatal(err)
			}
		}

		got, err := s.Evaluate(ctx, pct, at)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Breaching != 0 {
			t.Errorf("a percentage rule fired on an unlimited container at 25%% of the host — "+
				"%d samples breached, and none should have", got[0].Breaching)
		}

		got, err = s.Evaluate(ctx, cores, at)
		if err != nil {
			t.Fatal(err)
		}
		if !got[0].Sustained(20) {
			t.Fatalf("four cores flat for ten minutes did not sustain a 'CPU > 2 vCPU' rule "+
				"(%d of %d samples breaching)", got[0].Breaching, got[0].Samples)
		}
		// 25% of a 16-core allowance is 4 cores, and that is what the alert must report — the
		// derived metric is what is compared, so it had better be what is quoted too.
		if got[0].Worst < 3.99 || got[0].Worst > 4.01 {
			t.Errorf("the alert would report %.2f vCPU; the container is using 4", got[0].Worst)
		}
	})
}

// The absolute metrics have no natural ceiling, so zero is the threshold that has to be caught:
// with '>' it matches every sample for ever, with '<' it matches none, and both look like a
// working monitor from the outside.
func TestAnAbsoluteThresholdCannotBeZero(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		for _, metric := range []string{"mem_bytes", "cpu_cores"} {
			if err := s.CreateMonitor(ctx, &Monitor{
				Name: "x", Metric: metric, Op: ">", Threshold: 0, DurationSecs: 600,
			}); err == nil {
				t.Errorf("a %s monitor with a threshold of zero was accepted — it fires on "+
					"every sample of every container, for ever", metric)
			}
		}

		// But an ordinary absolute threshold, well past 100, is fine: these are quantities, not
		// percentages, and 2 GB is a perfectly reasonable line to draw.
		if err := s.CreateMonitor(ctx, &Monitor{
			Name: "Memory over 2GB", Metric: "mem_bytes", Op: ">",
			Threshold: 2 << 30, DurationSecs: 600,
		}); err != nil {
			t.Errorf("a 2 GB memory rule was rejected: %v", err)
		}
	})
}

// Sampling faster than the floor costs a `docker stats` call per container per round and answers
// no question the rules can ask — the shortest window a rule may have is 60 seconds.
func TestSamplingCannotBeFasterThanTheFloor(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		for _, secs := range []int{1, 5, 10, MinIntervalSecs - 1} {
			err := s.SaveMonitorSettings(ctx, &MonitorSettings{
				Enabled: true, IntervalSecs: secs, RetentionDays: 7,
			})
			if !errors.Is(err, ErrIntervalTooShort) {
				t.Errorf("a %ds sampling interval was accepted (err=%v) — the floor is %ds",
					secs, err, MinIntervalSecs)
			}
		}

		// The floor itself is allowed. An exclusive bound here would make the default unsaveable,
		// which is a fine way to have somebody discover that the form cannot be submitted at all.
		if err := s.SaveMonitorSettings(ctx, &MonitorSettings{
			Enabled: true, IntervalSecs: MinIntervalSecs, RetentionDays: 7,
		}); err != nil {
			t.Errorf("the minimum interval itself was rejected: %v", err)
		}
	})
}

// A row written before the floor existed still holds the old value. It must not keep sampling at
// that rate for ever, and — the nastier failure — it must not load into a settings form that then
// refuses to save ANY change, retention included, complaining about a field nobody touched.
func TestAnIntervalBelowTheFloorIsReadUpToIt(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		// Straight past the validation, the way a previous version of Daffa would have written it.
		if _, err := s.exec(ctx, `
            INSERT INTO monitor_settings (id, enabled, interval_secs, retention_days, updated_at)
            VALUES ('monitoring', 1, 10, 7, ?)`, ts(now())); err != nil {
			t.Fatal(err)
		}

		got, err := s.MonitorSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.IntervalSecs != MinIntervalSecs {
			t.Errorf("a stored interval of 10s was handed back as %ds — the collector would go on "+
				"sampling every ten seconds, and the settings form would load a value it cannot save",
				got.IntervalSecs)
		}
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────────

func writeSeries(t *testing.T, s *Store, env, name string, at time.Time, interval time.Duration, n int, v func(i int) float64) {
	t.Helper()

	samples := make([]Sample, 0, n)
	for i := range n {
		// Newest last; the whole run sits inside the window ending at `at`.
		ts := at.Add(-time.Duration(n-1-i) * interval)
		val := v(i)
		samples = append(samples, Sample{
			TS: ts, EnvID: env, ContainerID: "abc", ContainerName: name,
			CPUPct: val, MemPct: val, MemBytes: int64(val) * 1e6, MemLimit: 100e6,
		})
	}
	// One INSERT per timestamp: a round shares a timestamp, and the store writes a round.
	for _, m := range samples {
		if err := s.InsertSamples(context.Background(), []Sample{m}); err != nil {
			t.Fatal(err)
		}
	}
}

type hostFixture struct {
	store *Store
	env   string
}

func freshHost(t *testing.T, s *Store) hostFixture {
	t.Helper()
	e := &Environment{Name: "host-" + NewID()[:6]}
	if err := s.CreateEnvironment(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return hostFixture{store: s, env: e.ID}
}

func names(as []*Alert) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.ContainerName)
	}
	return out
}

func monitorNames(ms []*Monitor) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}
