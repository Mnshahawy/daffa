package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testDoc = `version: 1
name: my-app
cluster: prod
registries:
  - name: ghcr
    url: ghcr.io
    username: ci-bot
    password: { value_from_env: GHCR_TOKEN }
stacks:
  - name: api
    engine: compose
    source: { compose: 'services: {}' }
    env:
      - { key: LOG_LEVEL, value: info }
      - { key: SMTP_PASSWORD, secret: true, value_from_env: SMTP_PASSWORD }
  - name: edge
    engine: compose
    source: { compose: 'services: {}' }
`

func writeDoc(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func opts(verb, server, file string) ManifestOptions {
	return ManifestOptions{
		Verb:         verb,
		Server:       server,
		File:         file,
		Token:        "tok_test",
		Out:          &bytes.Buffer{},
		Log:          &bytes.Buffer{},
		PollInterval: time.Millisecond,
	}
}

func TestReportExit(t *testing.T) {
	cases := []struct {
		verb string
		sum  ReportSummary
		want int
	}{
		{"plan", ReportSummary{InSync: 5}, 0},
		{"plan", ReportSummary{}, 0},
		{"plan", ReportSummary{InSync: 5, Create: 1}, 2},
		{"plan", ReportSummary{Update: 1}, 2},
		{"plan", ReportSummary{Drifted: 1}, 2},
		{"plan", ReportSummary{Blocked: 1}, 2},
		{"plan", ReportSummary{Unfilled: 1}, 2},
		{"apply", ReportSummary{Create: 3, InSync: 2}, 0},
		{"apply", ReportSummary{Unfilled: 1}, 0},
		{"apply", ReportSummary{Drifted: 1}, 2},
		{"apply", ReportSummary{Blocked: 1}, 2},
	}
	for _, tc := range cases {
		if got := reportExit(tc.verb, tc.sum); got != tc.want {
			t.Errorf("reportExit(%s, %+v) = %d, want %d", tc.verb, tc.sum, got, tc.want)
		}
	}
}

// The plan round-trip: the document travels byte-identical, values ride beside it,
// the bearer token is stamped, and the exit code reflects the summary.
func TestPlanSubmitsDocumentAndValues(t *testing.T) {
	t.Setenv("GHCR_TOKEN", "gh-secret")
	t.Setenv("SMTP_PASSWORD", "smtp-secret")

	var got struct {
		Document string            `json:"document"`
		Values   map[string]string `json:"values"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/manifest/plan" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok_test" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(Report{
			Name:    "my-app",
			DocHash: "sha256:abc",
			Resources: []ReportResource{
				{Kind: "registry", Name: "ghcr", Verdict: "create"},
				{Kind: "stack", Name: "api", Cluster: "prod", Verdict: "in-sync"},
			},
			Summary: ReportSummary{Create: 1, InSync: 1},
		})
	}))
	defer srv.Close()

	o := opts("plan", srv.URL, writeDoc(t, testDoc))
	code, err := Manifest(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Errorf("exit = %d, want 2 (a create is pending)", code)
	}
	if got.Document != testDoc {
		t.Errorf("document was not sent byte-identical:\n%q", got.Document)
	}
	if got.Values["GHCR_TOKEN"] != "gh-secret" || got.Values["SMTP_PASSWORD"] != "smtp-secret" {
		t.Errorf("values = %v", got.Values)
	}

	out := o.Out.(*bytes.Buffer).String()
	for _, want := range []string{"registry", "ghcr", "create", "api (prod)", "1 to create"} {
		if !strings.Contains(out, want) {
			t.Errorf("report output is missing %q:\n%s", want, out)
		}
	}
}

func TestMissingValueFromEnvFailsBeforeTheNetwork(t *testing.T) {
	t.Setenv("SMTP_PASSWORD", "set")
	// GHCR_TOKEN deliberately unset — and the server must never be reached.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the server was reached despite a missing variable")
	}))
	defer srv.Close()

	code, err := Manifest(context.Background(), opts("plan", srv.URL, writeDoc(t, testDoc)))
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "GHCR_TOKEN") {
		t.Errorf("error does not name the missing variable: %v", err)
	}
}

func TestBrokenDocumentFailsBeforeTheNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the server was reached despite a broken document")
	}))
	defer srv.Close()

	code, err := Manifest(context.Background(),
		opts("plan", srv.URL, writeDoc(t, "version: 1\nstacks:\n  - {name: API, engine: compose, source: {compose: 'x: y'}}\n")))
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "manifest:") {
		t.Errorf("expected the validation error, got: %v", err)
	}
}

// applyServer fakes apply + deploy trigger + deployment polling. Deployments
// answer "running" once, then their final status.
func applyServer(t *testing.T, rep Report, finalStatus string, deployed *[]string) *httptest.Server {
	var polls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/manifest/apply":
			json.NewEncoder(w).Encode(rep)
		case strings.HasPrefix(r.URL.Path, "/api/stacks/") && strings.HasSuffix(r.URL.Path, "/up"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/stacks/"), "/up")
			*deployed = append(*deployed, id)
			json.NewEncoder(w).Encode(map[string]string{"deployment_id": "dep_" + id, "status": "running"})
		case strings.HasPrefix(r.URL.Path, "/api/deployments/"):
			status := finalStatus
			if polls.Add(1) == 1 {
				status = "running"
			}
			json.NewEncoder(w).Encode(map[string]string{"status": status, "log": "step one\nstep two\nboom"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestApplyDeployWalksStacksInDocumentOrder(t *testing.T) {
	t.Setenv("GHCR_TOKEN", "x")
	t.Setenv("SMTP_PASSWORD", "y")
	rep := Report{
		Resources: []ReportResource{
			{Kind: "stack", Name: "edge", Cluster: "prod", Verdict: "create", ID: "stk_edge"},
			{Kind: "stack", Name: "api", Cluster: "prod", Verdict: "create", ID: "stk_api"},
		},
		Summary: ReportSummary{Create: 2},
	}
	var deployed []string
	srv := applyServer(t, rep, "ok", &deployed)
	defer srv.Close()

	o := opts("apply", srv.URL, writeDoc(t, testDoc))
	o.Deploy = true
	code, err := Manifest(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("exit = %d", code)
	}
	// Document order — api before edge — regardless of report order.
	if len(deployed) != 2 || deployed[0] != "stk_api" || deployed[1] != "stk_edge" {
		t.Errorf("deployed = %v, want [stk_api stk_edge]", deployed)
	}
}

func TestApplyDeployStopsOnAFailedDeployment(t *testing.T) {
	t.Setenv("GHCR_TOKEN", "x")
	t.Setenv("SMTP_PASSWORD", "y")
	rep := Report{
		Resources: []ReportResource{
			{Kind: "stack", Name: "api", Cluster: "prod", Verdict: "create", ID: "stk_api"},
			{Kind: "stack", Name: "edge", Cluster: "prod", Verdict: "create", ID: "stk_edge"},
		},
	}
	var deployed []string
	srv := applyServer(t, rep, "failed", &deployed)
	defer srv.Close()

	o := opts("apply", srv.URL, writeDoc(t, testDoc))
	o.Deploy = true
	code, err := Manifest(context.Background(), o)
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error does not say the deployment failed: %v", err)
	}
	if len(deployed) != 1 {
		t.Errorf("the walk continued past a failure: %v", deployed)
	}
	if log := o.Log.(*bytes.Buffer).String(); !strings.Contains(log, "boom") {
		t.Errorf("the deploy log tail was not shown:\n%s", log)
	}
}

func TestApplyDeployRefusesUnfilledSlots(t *testing.T) {
	t.Setenv("GHCR_TOKEN", "x")
	t.Setenv("SMTP_PASSWORD", "y")
	rep := Report{
		Resources: []ReportResource{
			{Kind: "stack", Name: "api", Cluster: "prod", Verdict: "create", ID: "stk_api"},
		},
		Unfilled: []ReportUnfilled{{Kind: "stack_env", Stack: "api", Name: "DB_PASSWORD"}},
		Summary:  ReportSummary{Create: 1, Unfilled: 1},
	}
	var deployed []string
	srv := applyServer(t, rep, "ok", &deployed)
	defer srv.Close()

	o := opts("apply", srv.URL, writeDoc(t, testDoc))
	o.Deploy = true
	code, err := Manifest(context.Background(), o)
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Errorf("error does not name the unfilled slot: %v", err)
	}
	if len(deployed) != 0 {
		t.Errorf("deployed despite unfilled slots: %v", deployed)
	}
}

// A slot on ANOTHER cluster's same-named stack must not block this one; an entry
// with no cluster must (over-block beats under-block).
func TestUnfilledSlotClusterDisambiguation(t *testing.T) {
	t.Setenv("GHCR_TOKEN", "x")
	t.Setenv("SMTP_PASSWORD", "y")
	rep := Report{
		Resources: []ReportResource{
			{Kind: "stack", Name: "api", Cluster: "prod", Verdict: "in-sync", ID: "stk_api"},
			{Kind: "stack", Name: "edge", Cluster: "prod", Verdict: "in-sync", ID: "stk_edge"},
		},
		Unfilled: []ReportUnfilled{
			{Kind: "stack_env", Stack: "api", Cluster: "qa", Name: "DB_PASSWORD"},
		},
	}
	var deployed []string
	srv := applyServer(t, rep, "ok", &deployed)
	defer srv.Close()

	o := opts("apply", srv.URL, writeDoc(t, testDoc)) // testDoc's stacks default to cluster prod
	o.Deploy = true
	code, err := Manifest(context.Background(), o)
	if err != nil {
		t.Fatalf("a qa-cluster slot blocked a prod deploy: %v", err)
	}
	if code != 0 || len(deployed) != 2 {
		t.Errorf("code=%d deployed=%v", code, deployed)
	}

	// The same entry with no cluster blocks by name alone.
	rep.Unfilled[0].Cluster = ""
	deployed = nil
	srv2 := applyServer(t, rep, "ok", &deployed)
	defer srv2.Close()
	o2 := opts("apply", srv2.URL, writeDoc(t, testDoc))
	o2.Deploy = true
	code, err = Manifest(context.Background(), o2)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Fatalf("cluster-less slot did not block: code=%d err=%v", code, err)
	}
	if len(deployed) != 0 {
		t.Errorf("deployed despite the block: %v", deployed)
	}
}

func TestLogTail(t *testing.T) {
	if got := logTail("a\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("logTail = %q", got)
	}
	if got := logTail("", 2); got != "" {
		t.Errorf("logTail(empty) = %q", got)
	}
}
