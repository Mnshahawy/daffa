package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/manifest"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The reconciler is tested directly — manifestRun is the same walk the handlers use,
// minus HTTP — because the interesting claims are about semantics (idempotency, slots,
// drift, preflight), not about JSON plumbing.

func manifestServer(t *testing.T) (*Server, context.Context, *store.Environment) {
	t.Helper()
	s, ctx := certServer(t)
	env, node, err := s.store.UpsertLocalEnvironment(ctx, "local", "unix:///nonexistent.sock")
	if err != nil {
		t.Fatal(err)
	}
	// Registered but unreachable: placement and "is it connected" answer yes, and
	// anything that actually dials the daemon fails — which is what these tests want.
	if err := s.pool.Register(env, node); err != nil {
		t.Fatal(err)
	}
	return s, ctx, env
}

func manifestAdmin() *store.User {
	return &store.User{ID: "u_manifest", Caps: caps.ScopedMask{Global: caps.Of(
		caps.SSHKeysEdit, caps.RegistriesEdit, caps.GitCredsEdit, caps.NetworksEdit,
		caps.CertsEdit, caps.KeyringsEdit, caps.StacksEdit, caps.VolSourcesEdit,
	)}}
}

// runManifestDoc parses, preflights and walks a document, returning the report.
func runManifestDoc(t *testing.T, s *Server, ctx context.Context, u *store.User, doc string, values map[string]string, apply bool) manifestReport {
	t.Helper()
	m, err := manifest.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	run := &manifestRun{
		s: s, m: m, values: values, user: u, apply: apply,
		envs:   map[string]string{},
		report: manifestReport{Name: m.Name, DocHash: manifest.Hash([]byte(doc))},
	}
	if code, msg := run.preflight(ctx); code != "" {
		t.Fatalf("preflight refused: %s %s", code, msg)
	}
	if err := run.walk(ctx); err != nil {
		t.Fatal(err)
	}
	return run.report
}

func verdictOf(t *testing.T, rep manifestReport, kind, name string) manifestResourceView {
	t.Helper()
	for _, res := range rep.Resources {
		if res.Kind == kind && res.Name == name {
			return res
		}
	}
	t.Fatalf("no %s %q in report: %+v", kind, name, rep.Resources)
	return manifestResourceView{}
}

const testDoc = `
version: 1
name: test-app
cluster: local

ssh_keys:
  - { name: deploy-key }

registries:
  - name: ghcr
    url: ghcr.io
    username: bot
    password: { value_from_env: GHCR_TOKEN }

git_credentials:
  - name: repo
    kind: token
    username: bot
    token: { value_from_env: REPO_TOKEN }

cas:
  - { name: app-ca, common_name: App CA }

certificates:
  - name: api
    ca: app-ca
    sans: [api.internal]
    usages: [server, client]

keyrings:
  - { name: app-secrets, rotate_days: 30 }

cert_deliveries:
  - volume: app-certs
    certs: [{ name: api, default: true }]
    bundle_cas: [app-ca]

keyring_deliveries:
  - { keyring: app-secrets, volume: app-keyring }

stacks:
  - name: api
    engine: compose
    source:
      compose: |
        services:
          app:
            image: nginx
    auto_deploy: false
    env:
      - { key: LOG_LEVEL, value: info }
      - { key: DB_PASSWORD, secret: true }
      - { key: SMTP_PASSWORD, secret: true, value_from_env: SMTP_PASSWORD }

volume_sources:
  - volume: app-config
    source:
      files:
        - { path: app.yml, content: "a: 1\n" }
    stack: api
`

var testValues = map[string]string{
	"GHCR_TOKEN": "tok-1", "REPO_TOKEN": "tok-2", "SMTP_PASSWORD": "smtp-1",
}

// Idempotency IS the contract: the second apply of the same document reports
// everything in sync and writes nothing.
func TestManifestApplyIdempotent(t *testing.T) {
	s, ctx, env := manifestServer(t)
	u := manifestAdmin()

	first := runManifestDoc(t, s, ctx, u, testDoc, testValues, true)
	if got := first.Summary.Create; got != 10 {
		t.Fatalf("first apply created %d of 10 resources: %+v", got, first.Resources)
	}
	// The empty slot is reported; the CLI-filled one is not.
	if len(first.Unfilled) != 1 || first.Unfilled[0].Name != "DB_PASSWORD" || first.Unfilled[0].Cluster != "local" {
		t.Fatalf("unfilled: %+v", first.Unfilled)
	}

	second := runManifestDoc(t, s, ctx, u, testDoc, testValues, true)
	if second.Summary.Create != 0 || second.Summary.Update != 0 || second.Summary.Drifted != 0 || second.Summary.Blocked != 0 {
		t.Fatalf("second apply was not a no-op: %+v", second.Summary)
	}
	if second.Summary.InSync != 10 {
		t.Fatalf("second apply: %d in sync, want 10: %+v", second.Summary.InSync, second.Resources)
	}
	// The CLI's --deploy depends on ids being present on in-sync verdicts too.
	if verdictOf(t, second, "stack", "api").ID == "" {
		t.Fatal("in-sync stack carries no id")
	}

	// A value the operator typed into a slot survives further applies untouched.
	st, err := s.store.StackByName(ctx, env.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	vars, _ := s.store.StackEnv(ctx, st.ID)
	for i, v := range vars {
		if v.Key == "DB_PASSWORD" {
			sealed, _ := s.sealer.Seal("operator-typed")
			vars[i].ValueEnc = sealed
		}
	}
	if err := s.store.SetStackEnv(ctx, st.ID, vars); err != nil {
		t.Fatal(err)
	}
	third := runManifestDoc(t, s, ctx, u, testDoc, testValues, true)
	if len(third.Unfilled) != 0 {
		t.Fatalf("filled slot still reported unfilled: %+v", third.Unfilled)
	}
	vars, _ = s.store.StackEnv(ctx, st.ID)
	for _, v := range vars {
		if v.Key == "DB_PASSWORD" {
			got, _ := s.sealer.Open(v.ValueEnc)
			if got != "operator-typed" {
				t.Fatalf("apply overwrote a filled slot: %q", got)
			}
		}
	}
}

// Plan computes the same verdicts and writes NOTHING — and a self-consistent
// document on a fresh system reads as all-create, not blocked-on-itself: references
// the document itself declares are satisfied by walk order at apply time.
func TestManifestPlanTouchesNothing(t *testing.T) {
	s, ctx, _ := manifestServer(t)
	rep := runManifestDoc(t, s, ctx, manifestAdmin(), testDoc, testValues, false)
	if rep.Summary.Create != 10 || rep.Summary.Blocked != 0 {
		t.Fatalf("plan: %+v\n%+v", rep.Summary, rep.Resources)
	}
	if _, err := s.store.RegistryByName(ctx, "ghcr"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("plan created a registry")
	}
	if _, err := s.store.CertAuthorityByName(ctx, "app-ca"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("plan created a CA")
	}
	if list, _ := s.store.ListStacks(ctx, true, nil); len(list) != 0 {
		t.Fatal("plan created a stack")
	}
}

// A create-only credential with no resolvable value is BLOCKED, not created empty:
// a husk would move the failure to deploy time, where it is unrecognizable.
func TestManifestBlockedNotHusk(t *testing.T) {
	s, ctx, _ := manifestServer(t)
	doc := "version: 1\nregistries:\n  - {name: ghcr, url: ghcr.io, username: bot}\n"
	rep := runManifestDoc(t, s, ctx, manifestAdmin(), doc, nil, true)
	res := verdictOf(t, rep, "registry", "ghcr")
	if res.Verdict != verdictBlocked {
		t.Fatalf("verdict %q, want blocked", res.Verdict)
	}
	if _, err := s.store.RegistryByName(ctx, "ghcr"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a husk registry was created")
	}
}

// Trust material is never rotated: a differing CA is drift, and the stored row does
// not move.
func TestManifestDriftNeverRotates(t *testing.T) {
	s, ctx, _ := manifestServer(t)
	u := manifestAdmin()
	doc := "version: 1\ncas:\n  - {name: ca1, common_name: One}\n"
	runManifestDoc(t, s, ctx, u, doc, nil, true)
	before, err := s.store.CertAuthorityByName(ctx, "ca1")
	if err != nil {
		t.Fatal(err)
	}

	changed := "version: 1\ncas:\n  - {name: ca1, common_name: Two, key_algo: rsa-2048}\n"
	rep := runManifestDoc(t, s, ctx, u, changed, nil, true)
	if res := verdictOf(t, rep, "ca", "ca1"); res.Verdict != verdictDrifted {
		t.Fatalf("verdict %q, want drifted", res.Verdict)
	}
	after, _ := s.store.CertAuthorityByName(ctx, "ca1")
	if after.CertPEM != before.CertPEM || after.KeyAlgo != before.KeyAlgo {
		t.Fatal("drift rotated the CA")
	}
}

// value_from_env is declared intent: a new value overwrites the stored one, and the
// change reads as an update.
func TestManifestValueFromEnvOverwrites(t *testing.T) {
	s, ctx, env := manifestServer(t)
	u := manifestAdmin()
	doc := `
version: 1
cluster: local
stacks:
  - name: api
    engine: compose
    source: {compose: "services: {}"}
    env:
      - { key: TOKEN, secret: true, value_from_env: TOKEN }
`
	runManifestDoc(t, s, ctx, u, doc, map[string]string{"TOKEN": "v1"}, true)
	rep := runManifestDoc(t, s, ctx, u, doc, map[string]string{"TOKEN": "v2"}, true)
	if res := verdictOf(t, rep, "stack", "api"); res.Verdict != verdictUpdate {
		t.Fatalf("verdict %q, want update", res.Verdict)
	}
	st, _ := s.store.StackByName(ctx, env.ID, "api")
	vars, _ := s.store.StackEnv(ctx, st.ID)
	got, _ := s.sealer.Open(vars[0].ValueEnc)
	if got != "v2" {
		t.Fatalf("value %q, want v2", got)
	}

	// Without the variable, the filled slot is left alone — absence is not intent.
	rep = runManifestDoc(t, s, ctx, u, doc, nil, true)
	if res := verdictOf(t, rep, "stack", "api"); res.Verdict != verdictInSync {
		t.Fatalf("verdict %q, want in-sync", res.Verdict)
	}
}

// One missing capability refuses the WHOLE document before any mutation.
func TestManifestPreflightRefusesWholeDocument(t *testing.T) {
	s, ctx, _ := manifestServer(t)
	u := &store.User{ID: "u_certs", Caps: caps.ScopedMask{Global: caps.Of(caps.CertsEdit)}}
	doc := `
version: 1
cluster: local
cas:
  - { name: ca1, common_name: One }
stacks:
  - name: api
    engine: compose
    source: {compose: "services: {}"}
`
	m, _ := manifest.Parse([]byte(doc))
	run := &manifestRun{s: s, m: m, values: nil, user: u, apply: true, envs: map[string]string{}}
	code, msg := run.preflight(ctx)
	if code == "" {
		t.Fatal("preflight allowed a document the user may not fully apply")
	}
	if !strings.Contains(msg, "stacks.edit") {
		t.Fatalf("refusal does not name the missing capability: %q", msg)
	}
	if _, err := s.store.CertAuthorityByName(ctx, "ca1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused document mutated state")
	}
}

// An unknown cluster answers differently by privilege: a global holder gets an honest
// blocked verdict; anyone else cannot tell missing from forbidden.
func TestManifestUnknownClusterConflation(t *testing.T) {
	s, ctx, env := manifestServer(t)
	doc := `
version: 1
cluster: ghost
stacks:
  - name: api
    engine: compose
    source: {compose: "services: {}"}
`
	// Env-scoped holder: refusal, worded identically to "not yours".
	scoped := &store.User{ID: "u_scoped", Caps: caps.ScopedMask{
		Env: map[string]caps.Set{env.ID: caps.Of(caps.StacksEdit)},
	}}
	m, _ := manifest.Parse([]byte(doc))
	run := &manifestRun{s: s, m: m, user: scoped, envs: map[string]string{}}
	code, msg := run.preflight(ctx)
	if code == "" {
		t.Fatal("unknown cluster passed preflight for an env-scoped user")
	}
	if !strings.Contains(msg, "no such cluster exists") || !strings.Contains(msg, "do not hold it") {
		t.Fatalf("refusal must conflate missing and forbidden: %q", msg)
	}

	// Global holder: blocked verdict, honestly named.
	rep := runManifestDoc(t, s, ctx, manifestAdmin(), doc, nil, true)
	res := verdictOf(t, rep, "stack", "api")
	if res.Verdict != verdictBlocked || !strings.Contains(res.Detail, "no such cluster") {
		t.Fatalf("global holder: %+v", res)
	}
}

// Every manifest kind maps to a non-zero capability — adding a kind without deciding
// its authorization fails here, the same way an unguarded route would.
func TestManifestPreflightCoversEveryKind(t *testing.T) {
	for _, k := range manifest.Order {
		if c, _ := manifestCapFor(k); c == (caps.Cap{}) {
			t.Errorf("kind %q has no capability — decide its authorization in manifestCapFor", k)
		}
	}
}
