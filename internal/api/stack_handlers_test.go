package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/config"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// A secret name becomes daffa-secrets/<name> in the bundle and /run/secrets/<name> in the
// container, so anything that could escape that directory must be refused before it is written.
func TestValidSecretName(t *testing.T) {
	ok := []string{"db_password", "tls.key", "GOOGLE_CREDS", "a-b.c_d", "x"}
	for _, n := range ok {
		if !validSecretName(n) {
			t.Errorf("%q should be a valid secret name", n)
		}
	}
	bad := []string{"", "a/b", "..", "../escape", "a..b", `a\b`, "with space", "emoji😀", "a/../b"}
	for _, n := range bad {
		if validSecretName(n) {
			t.Errorf("%q must be refused as a secret name", n)
		}
	}
}

func stackServer(t *testing.T) (*Server, context.Context, *store.Store) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.DiscardHandler)
	s := &Server{store: st, pool: dockerx.NewPool(), notify: notify.New(st, fakeSealer{}, log)}
	return s, ctx, st
}

func inlineStack(t *testing.T, ctx context.Context, st *store.Store) *store.Stack {
	t.Helper()
	env := &store.Environment{Name: "prod"}
	if err := st.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	stack := &store.Stack{
		EnvID: env.ID, Name: "web", SourceKind: "inline",
		InlineYAML: "services:\n  app:\n    image: nginx:alpine\n",
	}
	if err := st.CreateStack(ctx, stack); err != nil {
		t.Fatal(err)
	}
	return stack
}

// updateStack drives handleUpdateStack directly with the stack already resolved into context, the
// way the scopeStack middleware would have left it.
func updateStack(s *Server, ctx context.Context, stack *store.Stack, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/stacks/"+stack.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withStack(ctx, stack))
	rec := httptest.NewRecorder()
	s.handleUpdateStack(rec, req)
	return rec
}

// A git-backed stack keeps its compose in the repo — there is nothing to import back. The switch is
// one-way, and the reverse must be refused with a reason rather than silently blanking the row.
func TestUpdateStackRefusesGitToInline(t *testing.T) {
	s, ctx, st := stackServer(t)

	env := &store.Environment{Name: "prod"}
	if err := st.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	stack := &store.Stack{
		EnvID: env.ID, Name: "web", SourceKind: "git",
		GitURL: "https://git.example.com/team/web.git", GitRef: "main", GitPath: "docker-compose.yml",
	}
	if err := st.CreateStack(ctx, stack); err != nil {
		t.Fatal(err)
	}

	rec := updateStack(s, ctx, stack, `{"source_kind":"inline","inline_yaml":"services: {}"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("switching a git stack back to inline returned %d; want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "inline stack can be switched to git") {
		t.Errorf("the refusal does not explain the one-way rule: %s", rec.Body)
	}

	// And the row is untouched: no half-applied switch.
	got, err := st.StackByID(ctx, stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceKind != "git" || got.GitURL == "" {
		t.Errorf("a refused switch mutated the stack: kind=%q url=%q", got.SourceKind, got.GitURL)
	}
}

// Switching to git without a URL cannot be probed and cannot be deployed — refuse it before the
// network probe, with the fix named.
func TestUpdateStackSwitchToGitRequiresURL(t *testing.T) {
	s, ctx, st := stackServer(t)
	stack := inlineStack(t, ctx, st)

	rec := updateStack(s, ctx, stack, `{"source_kind":"git","git_url":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("switching to git with a blank URL returned %d; want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "repository URL is required") {
		t.Errorf("the refusal does not name the missing URL: %s", rec.Body)
	}

	// The stack stays inline, with its YAML intact.
	got, err := st.StackByID(ctx, stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceKind != "inline" || got.InlineYAML == "" {
		t.Errorf("a refused switch changed the source: kind=%q yaml=%q", got.SourceKind, got.InlineYAML)
	}
}

// An update that does NOT change the source kind (e.g. renaming the group, or saving the inline
// compose) must skip the git probe entirely and persist — the switch logic is scoped to a genuine
// kind change and must not touch the common edit path.
func TestUpdateStackWithoutSwitchPersists(t *testing.T) {
	s, ctx, st := stackServer(t)
	stack := inlineStack(t, ctx, st)

	rec := updateStack(s, ctx, stack,
		`{"source_kind":"inline","group_name":"platform","inline_yaml":"services:\n  app:\n    image: caddy\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a same-kind update returned %d; want 200: %s", rec.Code, rec.Body)
	}

	got, err := st.StackByID(ctx, stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GroupName != "platform" {
		t.Errorf("the group was not persisted: %q", got.GroupName)
	}
	if !strings.Contains(got.InlineYAML, "caddy") {
		t.Errorf("the inline compose was not persisted: %q", got.InlineYAML)
	}
}

// registryAuths must hand the runner exactly the credentials for registries the compose actually
// pulls from — no more (a stored credential the stack never references leaks nothing to the
// runner) and no fewer (a multi-registry stack authenticates to each). It also has to carry a
// username-less credential through as a token, which dockerConfig then writes as a bearer.
func TestRegistryAuthsMatchesReferencedHostsOnly(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, err := config.NewMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := config.NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, sealer: sealer}

	seal := func(pw string) string {
		v, err := sealer.Seal(pw)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Three registries: two the compose references (one basic, one token-only), one it does not.
	if err := st.CreateRegistry(ctx, &store.Registry{Name: "priv", URL: "https://registry.example.com", Username: "deploy", PasswordEnc: seal("s3cret")}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRegistry(ctx, &store.Registry{Name: "ghcr", URL: "ghcr.io", Username: "", PasswordEnc: seal("ghp_token")}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRegistry(ctx, &store.Registry{Name: "unused", URL: "quay.io", Username: "u", PasswordEnc: seal("p")}); err != nil {
		t.Fatal(err)
	}

	yaml := "services:\n" +
		"  api:\n    image: registry.example.com/team/api:1.2\n" +
		"  worker:\n    image: ghcr.io/team/worker:latest\n" +
		"  cache:\n    image: redis:7\n" // public Docker Hub image — no credential

	auths, err := s.registryAuths(ctx, yaml, "proj", nil)
	if err != nil {
		t.Fatal(err)
	}

	byHost := map[string]*stacks.RegistryAuth{}
	for _, a := range auths {
		byHost[a.URL] = a
	}
	if len(byHost) != 2 {
		t.Fatalf("want creds for the 2 referenced private registries, got %d: %v", len(byHost), byHost)
	}
	if _, ok := byHost["quay.io"]; ok {
		t.Error("a registry the compose never references must not be handed to the runner")
	}
	if a := byHost["registry.example.com"]; a == nil || a.Username != "deploy" || a.Password != "s3cret" {
		t.Errorf("basic credential not resolved correctly: %+v", a)
	}
	// Username-less credential rides through with an empty Username so dockerConfig writes a token.
	if a := byHost["ghcr.io"]; a == nil || a.Username != "" || a.Password != "ghp_token" {
		t.Errorf("token credential not resolved correctly: %+v", a)
	}
}

// A public-image-only stack needs no credentials at all — no config.json, nothing unsealed.
func TestRegistryAuthsEmptyForPublicImages(t *testing.T) {
	s, ctx, st := stackServer(t)
	if err := st.CreateEnvironment(ctx, &store.Environment{Name: "prod"}); err != nil {
		t.Fatal(err)
	}

	auths, err := s.registryAuths(ctx, "services:\n  web:\n    image: nginx:alpine\n", "proj", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 0 {
		t.Errorf("a stack pulling only public images needs no credentials, got %d", len(auths))
	}
}

// setStackSecrets drives handleSetStackSecrets with the stack already in context.
func setStackSecrets(s *Server, ctx context.Context, stack *store.Stack, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/stacks/"+stack.ID+"/secrets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withStack(ctx, stack))
	rec := httptest.NewRecorder()
	s.handleSetStackSecrets(rec, req)
	return rec
}

// File secrets are Swarm-only: a compose stack's file: secret can't mount through the runner, so
// creating one is refused up front and the refusal names the compose path that works (env vars).
func TestSetStackSecretsRefusedOnCompose(t *testing.T) {
	s, ctx, st := stackServer(t)
	stack := inlineStack(t, ctx, st) // inlineStack is a compose stack

	rec := setStackSecrets(s, ctx, stack, `{"secrets":[{"name":"db_password","content":"hunter2"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("setting a secret on a compose stack returned %d; want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Swarm feature") ||
		!strings.Contains(rec.Body.String(), "environment variables") {
		t.Errorf("the refusal does not redirect to secret env vars: %s", rec.Body)
	}
}

// The same request on a Swarm stack is accepted — secrets there become raft secrets.
func TestSetStackSecretsAllowedOnSwarm(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key, err := config.NewMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := config.NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, sealer: sealer, notify: notify.New(st, fakeSealer{}, slog.New(slog.DiscardHandler))}

	env := &store.Environment{Name: "prod"}
	if err := st.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	stack := &store.Stack{EnvID: env.ID, Name: "web", Engine: "swarm", SourceKind: "inline",
		InlineYAML: "services:\n  app:\n    image: nginx\n"}
	if err := st.CreateStack(ctx, stack); err != nil {
		t.Fatal(err)
	}

	rec := setStackSecrets(s, ctx, stack, `{"secrets":[{"name":"db_password","content":"hunter2"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("setting a secret on a swarm stack returned %d; want 200: %s", rec.Code, rec.Body)
	}
	got, err := st.StackSecrets(ctx, stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "db_password" {
		t.Errorf("the secret was not stored: %+v", got)
	}
}

// checkSecretRefs splits by engine: compose refuses any daffa-secrets/ reference outright, swarm
// only refuses one with no stored secret behind it.
func TestCheckSecretRefsEngineAware(t *testing.T) {
	ctx := context.Background()
	yaml := "services:\n  db:\n    image: postgres\n    secrets: [x]\n" +
		"secrets:\n  x:\n    file: ./daffa-secrets/x\n"

	// Compose: refused regardless of whether a secret is stored, and it names the env-var path.
	err := checkSecretRefs(ctx, yaml, "proj", nil, []stacks.Secret{{Name: "x", Content: "v"}}, false)
	if err == nil || !strings.Contains(err.Error(), "Swarm feature") {
		t.Fatalf("compose must refuse a daffa-secrets reference with the Swarm-feature message: %v", err)
	}

	// Swarm without the stored secret: the classic "not defined" refusal.
	if err := checkSecretRefs(ctx, yaml, "proj", nil, nil, true); err == nil ||
		!strings.Contains(err.Error(), "not defined") {
		t.Fatalf("swarm must refuse an undefined secret: %v", err)
	}

	// Swarm with the stored secret: fine.
	if err := checkSecretRefs(ctx, yaml, "proj", nil, []stacks.Secret{{Name: "x", Content: "v"}}, true); err != nil {
		t.Errorf("swarm with the secret defined must pass: %v", err)
	}

	// A compose stack with no daffa-secrets reference is unaffected.
	plain := "services:\n  web:\n    image: nginx\n"
	if err := checkSecretRefs(ctx, plain, "proj", nil, nil, false); err != nil {
		t.Errorf("a compose stack with no file secrets must deploy: %v", err)
	}
}
