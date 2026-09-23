package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The limit is a ceiling going out and a floor coming in, and never a reason to move the
// other way: a service already past its ceiling is left alone, not scaled DOWN by a scale-out.
func TestScaleTarget(t *testing.T) {
	cases := []struct{ cur, by, limit, want int }{
		{2, 1, 5, 3},
		{4, 3, 5, 5},  // clamped to the ceiling
		{5, 1, 5, 5},  // at the ceiling: nothing
		{7, 1, 5, 7},  // already past it: nothing, not a scale-in
		{4, -1, 2, 3}, // scaling in
		{3, -5, 2, 2}, // clamped to the floor
		{1, -1, 2, 1}, // already below the floor: nothing, not a scale-out
	}
	for _, c := range cases {
		if got, _ := scaleTarget(c.cur, c.by, c.limit); got != c.want {
			t.Errorf("scaleTarget(%d, %+d, %d) = %d, want %d", c.cur, c.by, c.limit, got, c.want)
		}
	}
}

func TestMatchesAlertTarget(t *testing.T) {
	a := &store.Alert{ContainerName: "api", Stack: "billing"}
	if !matchesAlertTarget(dockerx.Container{Name: "billing-api-1", Service: "api", Project: "billing"}, a) {
		t.Error("a replica of the alerting service did not match")
	}
	if matchesAlertTarget(dockerx.Container{Name: "shop-api-1", Service: "api", Project: "shop"}, a) {
		t.Error("another stack's service of the same name matched")
	}
	solo := &store.Alert{ContainerName: "redis"}
	if !matchesAlertTarget(dockerx.Container{Name: "redis"}, solo) {
		t.Error("a standalone container did not match by name")
	}
}

// The HTTP action's body carries what a receiver needs, and is signed the way a forge's
// webhooks are, so one verifier serves both.
func TestHTTPActionPostsASignedAlert(t *testing.T) {
	s, _, _ := webhookServer(t)
	var got []byte
	var sig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		sig = r.Header.Get("X-Daffa-Signature")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sealed, _ := s.sealer.Seal("runbook-secret")
	act := store.MonitorAction{Kind: store.ActionHTTP, URL: srv.URL, SecretEnc: sealed}
	m := &store.Monitor{ID: "mon_1", Name: "leak", Metric: "mem_pct", Op: ">", Threshold: 90, DurationSecs: 600}
	a := &store.Alert{ID: "alr_1", ContainerName: "api", Stack: "billing", EnvID: "env_x", Value: 97, StartedAt: time.Now()}

	res := s.runAction(t.Context(), act, m, a, &actionTarget{})
	if !res.OK {
		t.Fatalf("http action: %+v", res)
	}
	mac := hmac.New(sha256.New, []byte("runbook-secret"))
	mac.Write(got)
	if sig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Errorf("signature %q does not verify", sig)
	}
	var p alertPayload
	if err := json.Unmarshal(got, &p); err != nil || p.Alert.Target != "api" || p.Monitor.Name != "leak" {
		t.Errorf("payload: %s (%v)", got, err)
	}
}

// Fire, act, resolve, refire inside the cooldown: the second firing records that it was
// skipped and why, instead of restarting again — the loop the cooldown exists to break.
func TestActionsRespectTheCooldown(t *testing.T) {
	s, st, _ := webhookServer(t)
	ctx := t.Context()
	env := &store.Environment{Name: "prod"}
	if err := st.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	m := &store.Monitor{Name: "leak", Enabled: true, Metric: "mem_pct", Op: ">", Threshold: 90,
		DurationSecs: 600, EnvID: env.ID, Actions: []store.MonitorAction{{Kind: store.ActionRestart}}}
	if err := st.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}

	first := &store.Alert{MonitorID: m.ID, EnvID: env.ID, ContainerName: "api"}
	_ = st.RaiseAlert(ctx, first)
	s.RunActions(ctx, m, first) // the cluster is not connected: recorded as a failed restart
	_ = st.ResolveAlert(ctx, first.ID, "back within the threshold")

	second := &store.Alert{MonitorID: m.ID, EnvID: env.ID, ContainerName: "api"}
	_ = st.RaiseAlert(ctx, second)
	s.RunActions(ctx, m, second)

	alerts, err := st.ListAlerts(ctx, true, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*store.Alert{}
	for _, a := range alerts {
		byID[a.ID] = a
	}
	if r := byID[first.ID].Actions; len(r) != 1 || r[0].OK || !strings.Contains(r[0].Detail, "not connected") {
		t.Errorf("first firing: %+v", r)
	}
	if r := byID[second.ID].Actions; len(r) != 1 || r[0].Kind != "cooldown" {
		t.Errorf("second firing inside the cooldown was not skipped: %+v", r)
	}
}

// A secret is write-only: saved without a new one, an HTTP action keeps the one it had —
// but only at the same URL. Re-pointing the action must not carry the secret along.
func TestActionSecretsSurviveAnEditButNotAMove(t *testing.T) {
	s, _, _ := webhookServer(t)
	prev := &store.Monitor{Actions: []store.MonitorAction{{Kind: store.ActionHTTP, URL: "https://a.lan/hook", SecretEnc: "sealed-a"}}}

	same := &store.Monitor{Actions: []store.MonitorAction{{Kind: store.ActionHTTP, URL: "https://a.lan/hook", HasSecret: true}}}
	rec := httptest.NewRecorder()
	if !s.sealActionSecrets(rec, httptest.NewRequest("PUT", "/", nil), same, prev) || same.Actions[0].SecretEnc != "sealed-a" {
		t.Errorf("an unchanged action lost its secret: %+v", same.Actions[0])
	}
	moved := &store.Monitor{Actions: []store.MonitorAction{{Kind: store.ActionHTTP, URL: "https://b.lan/hook", HasSecret: true}}}
	if !s.sealActionSecrets(rec, httptest.NewRequest("PUT", "/", nil), moved, prev) || moved.Actions[0].SecretEnc != "" {
		t.Errorf("the secret followed the action to a new URL: %+v", moved.Actions[0])
	}
	fresh := &store.Monitor{Actions: []store.MonitorAction{{Kind: store.ActionHTTP, URL: "https://b.lan/hook", Secret: "s3"}}}
	if !s.sealActionSecrets(rec, httptest.NewRequest("PUT", "/", nil), fresh, prev) ||
		fresh.Actions[0].SecretEnc == "" || fresh.Actions[0].Secret != "" {
		t.Errorf("a new secret was not sealed and cleared: %+v", fresh.Actions[0])
	}
}

// A monitor's actions are powers used later, unattended — creating one must take the same
// capability as doing it by hand, or monitors.edit becomes a way around containers.exec.
func TestActionsNeedThePowerTheyUse(t *testing.T) {
	env := "env_staging"
	operator := &store.User{Caps: caps.ScopedMask{Env: map[string]caps.Set{
		env: caps.Of(caps.MonitorsEdit, caps.ContainersEdit),
	}}}
	mon := func(kinds ...string) *store.Monitor {
		m := &store.Monitor{EnvID: env}
		for _, k := range kinds {
			m.Actions = append(m.Actions, store.MonitorAction{Kind: k})
		}
		return m
	}
	if _, _, _, ok := missingActionCap(operator, mon(store.ActionRestart)); !ok {
		t.Error("containers.edit could not arm a restart")
	}
	for kind, want := range map[string]caps.Cap{
		store.ActionExec:  caps.ContainersExec,
		store.ActionScale: caps.ServicesEdit,
		store.ActionHTTP:  caps.SettingsEdit,
	} {
		if _, c, _, ok := missingActionCap(operator, mon(store.ActionRestart, kind)); ok || c != want {
			t.Errorf("%s without %s: ok=%v missing=%s", kind, want.Name(), ok, c.Name())
		}
	}
	// settings.edit on the cluster is not settings.edit fleet-wide.
	scopedAdmin := &store.User{Caps: caps.ScopedMask{Env: map[string]caps.Set{env: caps.Of(caps.SettingsEdit)}}}
	if _, _, global, ok := missingActionCap(scopedAdmin, mon(store.ActionHTTP)); ok || !global {
		t.Error("an HTTP action was armed without settings.edit fleet-wide")
	}
}
