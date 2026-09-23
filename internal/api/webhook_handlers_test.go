package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/config"
	"github.com/Mnshahawy/daffa/internal/store"
)

// One repository, every form the forges send for it, must reduce to one key — or a stack
// configured with the ssh URL silently ignores pushes whose payload leads with https.
func TestRepoKey(t *testing.T) {
	same := []string{
		"https://github.com/Acme/App.git",
		"https://github.com/acme/app",
		"https://github.com/acme/app/",
		"git@github.com:acme/app.git",
		"ssh://git@github.com:22/acme/app.git",
		"git://github.com/acme/app.git",
		"https://user:token@github.com/acme/app.git",
	}
	for _, u := range same {
		if got := repoKey(u); got != "github.com/acme/app" {
			t.Errorf("repoKey(%q) = %q, want github.com/acme/app", u, got)
		}
	}
	// A self-hosted forge serves ssh and https on different ports for the same repo.
	if a, b := repoKey("ssh://git@forge.lan:2222/team/svc.git"), repoKey("https://forge.lan:3000/team/svc"); a != b {
		t.Errorf("ports split one repository: %q vs %q", a, b)
	}
	// Subgroups are part of the identity, not noise.
	if repoKey("https://gitlab.com/org/sub/app.git") == repoKey("https://gitlab.com/org/app.git") {
		t.Error("a GitLab subgroup collapsed into its parent")
	}
	for _, u := range []string{"", "not a url", "/srv/git/app.git"} {
		if got := repoKey(u); got != "" {
			t.Errorf("repoKey(%q) = %q, want empty", u, got)
		}
	}
}

func TestParsePushCollectsRepositories(t *testing.T) {
	github := `{"ref":"refs/heads/main","repository":{"clone_url":"https://github.com/acme/app.git",
		"ssh_url":"git@github.com:acme/app.git","url":"https://api.github.com/repos/acme/app"},
		"commits":[{"added":["a.yml"],"modified":["b.yml"],"removed":[]}]}`
	p, err := parsePush([]byte(github))
	if err != nil {
		t.Fatal(err)
	}
	// The API URL identifies nothing a stack is configured with; it must not appear.
	if len(p.Repos) != 1 || p.Repos[0] != "github.com/acme/app" {
		t.Errorf("GitHub repos = %v", p.Repos)
	}
	if len(p.Files) != 2 {
		t.Errorf("files = %v", p.Files)
	}

	gitlab := `{"ref":"refs/heads/main","project":{"git_http_url":"https://gitlab.com/org/app.git",
		"git_ssh_url":"git@gitlab.com:org/app.git","web_url":"https://gitlab.com/org/app"}}`
	if p, _ = parsePush([]byte(gitlab)); len(p.Repos) != 1 || p.Repos[0] != "gitlab.com/org/app" {
		t.Errorf("GitLab repos = %v", p.Repos)
	}
}

// The per-target rules, which are the whole feature: one push, several targets, each
// deciding for itself.
func TestPlanDelivery(t *testing.T) {
	repo := "https://github.com/acme/platform.git"
	api := &store.Stack{ID: "s1", Name: "api", SourceKind: "git", GitURL: repo, GitRef: "main",
		GitPath: "api/compose.yml", AutoDeploy: true}
	web := &store.Stack{ID: "s2", Name: "web", SourceKind: "git", GitURL: repo, GitRef: "main",
		GitPath: "web/compose.yml", AutoDeploy: true}
	staging := &store.Stack{ID: "s3", Name: "api-staging", SourceKind: "git", GitURL: repo, GitRef: "develop",
		GitPath: "api/compose.yml", AutoDeploy: true}
	paused := &store.Stack{ID: "s4", Name: "paused", SourceKind: "git", GitURL: repo, GitRef: "main",
		GitPath: "api/compose.yml", AutoDeploy: false}
	other := &store.Stack{ID: "s5", Name: "other", SourceKind: "git", GitURL: "git@github.com:acme/other.git",
		GitRef: "main", GitPath: "api/compose.yml", AutoDeploy: true}
	// A source whose stack deploys in this same delivery is synced BY that deploy.
	covered := &store.VolumeSource{ID: "v1", Volume: "api-config", SourceKind: "git", GitURL: repo,
		GitRef: "main", GitPath: "api/config", AutoSync: true, StackID: "s1"}
	alone := &store.VolumeSource{ID: "v2", Volume: "proxy", SourceKind: "git", GitURL: repo,
		GitRef: "main", GitPath: "proxy", AutoSync: true}

	p := &push{Ref: "refs/heads/main", Repos: []string{"github.com/acme/platform"},
		Files: []string{"api/compose.yml", "api/config/app.yml", "proxy/traefik.yml"}}
	got := map[string]string{}
	for _, r := range planDelivery(p, []*store.Stack{api, web, staging, paused, other},
		[]*store.VolumeSource{covered, alone}) {
		got[r.Name] = r.Status
	}
	want := map[string]string{
		"api":         "deploying", // its compose file changed
		"web":         "ignored",   // same repo, nothing it watches changed
		"api-staging": "ignored",   // tracks develop
		"paused":      "ignored",   // linked, but auto-deploy is off
		"other":       "ignored",   // another repository on an org-level hook
		"api-config":  "covered",   // api's deploy syncs it first
		"proxy":       "syncing",   // its subtree changed, no stack deploying it
	}
	for name, status := range want {
		if got[name] != status {
			t.Errorf("%s: %q, want %q", name, got[name], status)
		}
	}

	// A payload that names no repository is not filtered — act rather than miss.
	p.Repos = nil
	for _, r := range planDelivery(p, []*store.Stack{other}, nil) {
		if r.Status != "deploying" {
			t.Errorf("a repo-less payload was filtered: %+v", r)
		}
	}
}

func webhookServer(t *testing.T) (*Server, *store.Store, *http.ServeMux) {
	t.Helper()
	s, _, st := stackServer(t)
	sealer, err := config.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.sealer = sealer
	s.cfg = &config.Config{} // clientIP reads TrustProxy from it
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/{id}", s.handleWebhookDelivery)
	mux.HandleFunc("POST /webhooks/stacks/{id}", s.handleLegacyStackWebhook)
	return s, st, mux
}

func signed(t *testing.T, url, secret, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

// The receive path end to end, on a paused target so nothing deploys: the signature is the
// authentication, the verdict names the target, the delivery is recorded, and the URL a
// forge was configured with before webhooks were objects still reaches the same webhook.
func TestWebhookDelivery(t *testing.T) {
	s, st, mux := webhookServer(t)
	ctx := t.Context()

	env := &store.Environment{Name: "prod"}
	if err := st.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	sealed, _ := s.sealer.Seal("s3cret")
	wh := &store.Webhook{Name: "platform", SecretEnc: sealed}
	if err := st.CreateWebhook(ctx, wh); err != nil {
		t.Fatal(err)
	}
	stack := &store.Stack{EnvID: env.ID, Name: "api", SourceKind: "git",
		GitURL: "https://github.com/acme/platform.git", GitRef: "main", GitPath: "compose.yml"}
	if err := st.CreateStack(ctx, stack); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStackAutoDeploy(ctx, stack.ID, false, wh.ID, ""); err != nil {
		t.Fatal(err)
	}
	body := `{"ref":"refs/heads/main","repository":{"clone_url":"https://github.com/acme/platform.git"},
		"commits":[{"modified":["compose.yml"]}]}`

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signed(t, "/webhooks/"+wh.ID, "wrong", body))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a bad signature answered %d, want 401", rec.Code)
	}

	for _, url := range []string{"/webhooks/" + wh.ID, "/webhooks/stacks/" + stack.ID} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, signed(t, url, "s3cret", body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", url, rec.Code, rec.Body)
		}
		var resp struct {
			Status  string           `json:"status"`
			Results []deliveryResult `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != "ignored" || len(resp.Results) != 1 || !strings.Contains(resp.Results[0].Reason, "paused") {
			t.Errorf("%s: %+v", url, resp)
		}
	}

	got, err := st.WebhookByID(ctx, wh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastDeliveryAt.IsZero() || !strings.Contains(got.LastDelivery, "paused") {
		t.Errorf("last delivery not recorded: %q", got.LastDelivery)
	}

	// A stack with no webhook has no legacy URL either.
	lone := &store.Stack{EnvID: env.ID, Name: "lone", SourceKind: "git", GitURL: "https://x/y.git"}
	if err := st.CreateStack(ctx, lone); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, signed(t, "/webhooks/stacks/"+lone.ID, "s3cret", body))
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unlinked stack's legacy URL answered %d, want 404", rec.Code)
	}
}

// A push through a staging-only webhook must not be able to deploy prod.
func TestLinkableWebhook(t *testing.T) {
	s, st, _ := webhookServer(t)
	ctx := t.Context()
	staging := &store.Environment{Name: "staging"}
	prod := &store.Environment{Name: "prod"}
	for _, e := range []*store.Environment{staging, prod} {
		if err := st.CreateEnvironment(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	scoped := &store.Webhook{Name: "staging-only", EnvID: staging.ID, SecretEnc: "x"}
	fleet := &store.Webhook{Name: "fleet", SecretEnc: "x"}
	for _, w := range []*store.Webhook{scoped, fleet} {
		if err := st.CreateWebhook(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.linkableWebhook(ctx, scoped.ID, prod.ID); err == nil {
		t.Error("a prod stack linked a staging-only webhook")
	}
	if id, err := s.linkableWebhook(ctx, scoped.ID, staging.ID); err != nil || id != scoped.ID {
		t.Errorf("same-cluster link refused: %v", err)
	}
	if id, err := s.linkableWebhook(ctx, fleet.ID, prod.ID); err != nil || id != fleet.ID {
		t.Errorf("fleet-wide link refused: %v", err)
	}
}
