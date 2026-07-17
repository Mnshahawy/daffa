package monitor

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// A host that has just run out of memory has a monitor matching every container on it, and all
// of them breach in the same round.
//
// Naively that is one email per container. And a hundred emails is functionally identical to no
// email at all, because nobody reads any of them — so the alert that was supposed to save the
// night is the thing that ruins it.
func TestAMonitorSendsOneMailForManyContainers(t *testing.T) {
	ctx := context.Background()
	s, env := testStore(t)

	// Somebody is listening.
	if err := s.SaveSMTPSettings(ctx, &store.SMTPSettings{
		Host: "smtp.example.com", Port: 587, FromAddr: "daffa@example.com", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNotificationRule(ctx, &store.NotificationRule{
		Event: string(notify.MonitorFired), Address: "oncall@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	m := &store.Monitor{
		Name: "Memory high", Enabled: true, Metric: "mem_pct", Op: ">",
		Threshold: 70, DurationSecs: 600, EnvID: env,
	}
	if err := s.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}

	// Twenty containers, all of them over the line for the whole window.
	at := time.Now().UTC().Truncate(time.Second)
	const interval = 30 * time.Second
	for i := range 20 {
		writeBreach(t, s, env, containerNameFor(i), at, interval, 20)
	}

	e := NewEvaluator(s, notify.New(s, fakeSealer{}, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	e.Run(ctx, at)

	// Twenty alerts were raised...
	alerts, err := s.ListAlerts(ctx, true, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 20 {
		t.Fatalf("raised %d alerts; want 20 — every container breached", len(alerts))
	}

	// ...and exactly ONE email went out.
	due, err := s.DueMessages(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("twenty breaching containers produced %d emails; want 1. A hundred-email "+
			"alert is the same as no alert, because nobody reads any of them.", len(due))
	}

	// And that one message names them all, or it has coalesced away the information it exists
	// to carry.
	body := due[0].Text
	for _, want := range []string{"api-0", "api-7", "api-19"} {
		if !strings.Contains(body, want) {
			t.Errorf("the coalesced mail does not mention %s:\n%s", want, body)
		}
	}
	if !strings.Contains(due[0].Subject, "20 containers") {
		t.Errorf("subject = %q; want it to say how many", due[0].Subject)
	}

	// A second round with the containers still breaching must NOT mail again. A monitor that
	// re-pages every thirty seconds gets muted, and then it is not a monitor.
	e.Run(ctx, at)
	due, _ = s.DueMessages(ctx, 100)
	if len(due) != 1 {
		t.Errorf("a still-firing monitor mailed again on the next round: %d messages", len(due))
	}
}

// The one that scoping pays for: a monitor watching production must not page a staging-scoped
// operator, and a fleet-wide monitor must page each host's people about their own host.
func TestAFleetWideMonitorMailsEachHostsPeopleSeparately(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	prod := mustEnv(t, s, "prod")
	staging := mustEnv(t, s, "staging")

	if err := s.SaveSMTPSettings(ctx, &store.SMTPSettings{
		Host: "smtp.example.com", Port: 587, FromAddr: "daffa@example.com", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Sara operates staging; Raj operates prod. The rule pages whoever holds Operator.
	op, err := s.RoleByName(ctx, "Operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ name, email, env string }{
		{"sara", "sara@example.com", staging},
		{"raj", "raj@example.com", prod},
	} {
		user := &store.User{Kind: "local", Username: u.name, Email: u.email}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, user.ID, op.ID, store.SourceLocal, store.OnEnv(u.env)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateNotificationRule(ctx, &store.NotificationRule{
		Event: string(notify.MonitorFired), RoleID: op.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// One FLEET-WIDE monitor: no host filter at all.
	m := &store.Monitor{
		Name: "Memory high", Enabled: true, Metric: "mem_pct", Op: ">",
		Threshold: 70, DurationSecs: 600,
	}
	if err := s.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}

	// A container on each host breaches, in the same round.
	at := time.Now().UTC().Truncate(time.Second)
	writeBreach(t, s, prod, "prod-api-1", at, 30*time.Second, 20)
	writeBreach(t, s, staging, "staging-api-1", at, 30*time.Second, 20)

	e := NewEvaluator(s, notify.New(s, fakeSealer{}, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	e.Run(ctx, at)

	due, err := s.DueMessages(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}

	// Two messages: one per host, to that host's operator. NOT one message to both of them,
	// which is exactly how a staging-scoped operator ends up reading production container names.
	got := map[string]string{}
	for _, msg := range due {
		got[msg.To] = msg.Text
	}
	if len(got) != 2 {
		t.Fatalf("a fleet-wide monitor breaching on two hosts sent %d messages; want 2 — "+
			"one per host, to that host's people", len(due))
	}

	if !strings.Contains(got["raj@example.com"], "prod-api-1") {
		t.Errorf("the prod operator was not told about the prod container")
	}
	if strings.Contains(got["raj@example.com"], "staging-api-1") {
		t.Errorf("the prod operator's mail names a STAGING container")
	}
	if !strings.Contains(got["sara@example.com"], "staging-api-1") {
		t.Errorf("the staging operator was not told about the staging container")
	}
	if strings.Contains(got["sara@example.com"], "prod-api-1") {
		t.Errorf("the staging operator's mail names a PRODUCTION container. Coalescing per " +
			"monitor rather than per HOST is how that happens, and it is the leak scoping " +
			"exists to prevent.")
	}
}

// A container that goes away resolves its alert, rather than leaving it firing forever on
// something that no longer exists.
func TestAVanishedContainerResolvesItsAlert(t *testing.T) {
	ctx := context.Background()
	s, env := testStore(t)

	m := &store.Monitor{
		Name: "Memory high", Enabled: true, Metric: "mem_pct", Op: ">",
		Threshold: 70, DurationSecs: 600, EnvID: env,
	}
	if err := s.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	writeBreach(t, s, env, "api", at, 30*time.Second, 20)

	e := NewEvaluator(s, notify.New(s, fakeSealer{}, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	e.Run(ctx, at)

	if _, err := s.FiringAlert(ctx, m.ID, "api"); err != nil {
		t.Fatalf("the alert did not fire: %v", err)
	}

	// The container is redeployed and stops reporting. Three intervals later, nothing.
	e.Run(ctx, at.Add(5*time.Minute))

	if _, err := s.FiringAlert(ctx, m.ID, "api"); err == nil {
		t.Error("a container that stopped reporting five minutes ago is still firing. An " +
			"alert that hangs forever on a container that no longer exists is the one that " +
			"teaches everybody to ignore the page.")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────────

type fakeSealer struct{}

func (fakeSealer) Open(s string) (string, error) { return s, nil }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustEnv(t *testing.T, s *store.Store, name string) string {
	t.Helper()
	e := &store.Environment{Name: name}
	if err := s.CreateEnvironment(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e.ID
}

func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	s := openStore(t)
	return s, mustEnv(t, s, "prod")
}

// writeBreach lays down n samples for one container, all of them over 70%.
func writeBreach(t *testing.T, s *store.Store, env, name string, at time.Time, interval time.Duration, n int) {
	t.Helper()
	for i := range n {
		ts := at.Add(-time.Duration(n-1-i) * interval)
		if err := s.InsertSamples(context.Background(), []store.Sample{{
			TS: ts, EnvID: env, ContainerID: "abc", ContainerName: name,
			CPUPct: 90, MemPct: 85, MemBytes: 850e6, MemLimit: 1000e6,
		}}); err != nil {
			t.Fatal(err)
		}
	}
}

func containerNameFor(i int) string {
	return "api-" + itoa(i)
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// A stopped container must not resolve as "recovered", and the reason is a bug I shipped and
// then watched happen against a real daemon.
//
// When a container stops, its samples age out of the evaluation window. Coverage collapses,
// "sustained" goes false — and if that is read as recovery, the alert resolves itself with the
// words "back within the threshold (100%)". A sentence that contradicts itself, about a
// container that no longer exists. An operator who reads that once stops believing the next
// number the tool shows them, and they are right to.
//
// Absence of evidence is not evidence of recovery. Silence is resolved AS silence.
func TestSilenceIsNotRecovery(t *testing.T) {
	ctx := context.Background()
	s, env := testStore(t)

	m := &store.Monitor{
		Name: "CPU high", Enabled: true, Metric: "cpu_pct", Op: ">",
		Threshold: 70, DurationSecs: 600, EnvID: env,
	}
	if err := s.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	const interval = 30 * time.Second

	// Ten minutes of a container pegged at 90%, and it fires.
	writeBreach(t, s, env, "api", at, interval, 20)
	e := NewEvaluator(s, notify.New(s, fakeSealer{}, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	e.Run(ctx, at)

	if _, err := s.FiringAlert(ctx, m.ID, "api"); err != nil {
		t.Fatalf("the alert did not fire: %v", err)
	}

	// Now the container STOPS. No new samples — and we evaluate at a moment when only a couple
	// of the old ones are still inside the window. That is exactly the state that used to be
	// misread as recovery.
	//
	// 18 of the 20 samples have aged out; the 2 that remain are still at 90%, still breaching.
	// There is no version of the truth in which this container is "back within the threshold".
	e.Run(ctx, at.Add(9*time.Minute+30*time.Second))

	alerts, err := s.ListAlerts(ctx, true, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts; want 1", len(alerts))
	}

	if alerts[0].State == store.AlertResolved &&
		strings.Contains(alerts[0].ResolveReason, "back within the threshold") {
		t.Fatalf("a container that STOPPED was resolved as recovered: %q.\n\n"+
			"Its last samples were at 90%%, above a 70%% threshold. Falling below the coverage "+
			"floor is not the same as coming back under the line, and saying so produces a "+
			"self-contradicting sentence that costs you the operator's trust.",
			alerts[0].ResolveReason)
	}

	// Long enough for the silence to be unambiguous: it resolves, and it says why.
	e.Run(ctx, at.Add(30*time.Minute))

	alerts, _ = s.ListAlerts(ctx, true, nil, 10)
	if alerts[0].State != store.AlertResolved {
		t.Fatal("a container that has been silent for twenty minutes is still firing")
	}
	if !strings.Contains(alerts[0].ResolveReason, "stopped reporting") {
		t.Errorf("resolved with %q; want it to say the container stopped reporting",
			alerts[0].ResolveReason)
	}
}

// And a genuine recovery reports the value it came back TO, not the worst it ever was.
func TestRecoveryReportsWhatItRecoveredTo(t *testing.T) {
	ctx := context.Background()
	s, env := testStore(t)

	m := &store.Monitor{
		Name: "CPU high", Enabled: true, Metric: "cpu_pct", Op: ">",
		Threshold: 70, DurationSecs: 600, EnvID: env,
	}
	if err := s.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	const interval = 30 * time.Second

	writeBreach(t, s, env, "api", at, interval, 20) // pegged at 90%, fires
	e := NewEvaluator(s, notify.New(s, fakeSealer{}, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	e.Run(ctx, at)

	// It calms down: three samples at 12%, well under the line. The window still has plenty of
	// the old 90% ones in it, so coverage is fine and the recovery is real.
	for i := 1; i <= 3; i++ {
		if err := s.InsertSamples(ctx, []store.Sample{{
			TS: at.Add(time.Duration(i) * interval), EnvID: env, ContainerName: "api",
			CPUPct: 12, MemPct: 20, MemBytes: 200e6, MemLimit: 1000e6,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	e.Run(ctx, at.Add(3*interval))

	alerts, _ := s.ListAlerts(ctx, true, nil, 10)
	if alerts[0].State != store.AlertResolved {
		t.Fatal("a container back under the threshold is still firing")
	}
	// 12%, the value it came back to — NOT 90%, the worst it ever was.
	if !strings.Contains(alerts[0].ResolveReason, "12%") {
		t.Errorf("resolved with %q; want it to quote 12%% — the value it recovered TO. "+
			"Quoting the window's worst reading produces \"back within the threshold (90%%)\", "+
			"which is a sentence that argues with itself.", alerts[0].ResolveReason)
	}
}
