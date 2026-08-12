package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Mnshahawy/daffa/internal/manifest"
)

// ManifestOptions drives `daffa plan` and `daffa apply` — one option set, because
// the two verbs differ only in which endpoint they hit and how their exit code
// reads the report.
type ManifestOptions struct {
	Verb     string // "plan" or "apply"
	Server   string
	File     string
	Username string
	Password string
	Token    string
	Insecure bool
	// Deploy makes apply walk the manifest's stacks in DOCUMENT order after a
	// successful apply, deploying each and waiting for it before the next. The
	// server never learns a DAG — the document's order is the deploy order.
	Deploy bool

	Out io.Writer // the report; stdout when nil
	Log io.Writer // progress; stderr when nil

	// PollInterval is how often --deploy polls a running deployment. The default
	// suits a human watching; tests shrink it.
	PollInterval time.Duration
}

// The report shapes mirror the server's plan/apply responses.

// ReportResource is one resource's verdict.
type ReportResource struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Cluster string `json:"cluster"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail"`
	ID      string `json:"id"`
}

// ReportUnfilled is one secret slot with no value yet.
type ReportUnfilled struct {
	Kind    string `json:"kind"` // stack_env, stack_secret_file, or a resource kind
	Stack   string `json:"stack"`
	Cluster string `json:"cluster"` // the stack's cluster; may be absent
	Name    string `json:"name"`
}

// ReportSummary counts verdicts.
type ReportSummary struct {
	Create   int `json:"create"`
	Update   int `json:"update"`
	InSync   int `json:"in_sync"`
	Drifted  int `json:"drifted"`
	Blocked  int `json:"blocked"`
	Unfilled int `json:"unfilled"`
}

// Report is the server's answer to plan and apply.
type Report struct {
	Name      string           `json:"name"`
	DocHash   string           `json:"doc_hash"`
	Resources []ReportResource `json:"resources"`
	Unfilled  []ReportUnfilled `json:"unfilled"`
	Summary   ReportSummary    `json:"summary"`
}

// Manifest runs plan or apply and returns the process exit code. An error means
// exit 1; otherwise the code comes from reportExit, so CI can read divergence
// without parsing output.
func Manifest(ctx context.Context, o ManifestOptions) (int, error) {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.Server == "" {
		return 1, fmt.Errorf("%s: --server is required (or set DAFFA_SERVER)", o.Verb)
	}
	if o.File == "" {
		return 1, fmt.Errorf("%s: -f is required — the path to the manifest file", o.Verb)
	}
	o.Server = strings.TrimSuffix(o.Server, "/")

	doc, err := os.ReadFile(o.File)
	if err != nil {
		return 1, fmt.Errorf("%s: reading the manifest: %w", o.Verb, err)
	}

	// Parse and validate HERE, before any network call, so a broken document fails
	// fast with the full joined error list rather than one round-trip per mistake.
	m, err := manifest.Parse(doc)
	if err != nil {
		return 1, err
	}
	if err := m.Validate(); err != nil {
		return 1, err
	}

	// Resolve every value_from_env from THIS process's environment. The values ride
	// beside the document, never inside it — the server stores the document
	// byte-identical to the file on disk.
	values, missing := resolveValues(m)
	if len(missing) > 0 {
		return 1, fmt.Errorf("%s: these value_from_env variables are not set: %s — export them and re-run",
			o.Verb, strings.Join(missing, ", "))
	}

	client, err := connect(ctx, connectOptions{
		server:   o.Server,
		username: o.Username,
		password: o.Password,
		token:    o.Token,
		insecure: o.Insecure,
	})
	if err != nil {
		return 1, fmt.Errorf("%s: %w", o.Verb, err)
	}

	rep, err := submit(ctx, client, o, doc, values)
	if err != nil {
		return 1, err
	}
	printReport(o.Out, rep)

	if o.Verb == "apply" && o.Deploy {
		if err := deployWalk(ctx, client, o, m, rep); err != nil {
			return 1, err
		}
	}
	return reportExit(o.Verb, rep.Summary), nil
}

// resolveValues collects every value_from_env in the document and looks each up in
// the environment. It returns the resolved values and the sorted list of variables
// that are not set — reported all at once, named, so the operator fixes the
// environment in one pass.
func resolveValues(m *manifest.Manifest) (map[string]string, []string) {
	var names []string
	for _, r := range m.Registries {
		if r.Password != nil && r.Password.ValueFromEnv != "" {
			names = append(names, r.Password.ValueFromEnv)
		}
	}
	for _, g := range m.GitCredentials {
		if g.Token != nil && g.Token.ValueFromEnv != "" {
			names = append(names, g.Token.ValueFromEnv)
		}
	}
	for _, st := range m.Stacks {
		for _, e := range st.Env {
			if e.ValueFromEnv != "" {
				names = append(names, e.ValueFromEnv)
			}
		}
	}

	values := map[string]string{}
	var missing []string
	for _, n := range names {
		if _, ok := values[n]; ok {
			continue
		}
		if v, ok := os.LookupEnv(n); ok {
			values[n] = v
		} else {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	return values, dedupe(missing)
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || sorted[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func submit(ctx context.Context, client *http.Client, o ManifestOptions, doc []byte, values map[string]string) (*Report, error) {
	body, err := json.Marshal(struct {
		Document string            `json:"document"`
		Values   map[string]string `json:"values,omitempty"`
	}{Document: string(doc), Values: values})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Server+"/api/manifest/"+o.Verb, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "daffa-cli")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Verb, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", o.Verb, apiError(resp))
	}
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, fmt.Errorf("%s: reading the report: %w", o.Verb, err)
	}
	return &rep, nil
}

func printReport(w io.Writer, rep *Report) {
	if len(rep.Resources) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "KIND\tNAME\tVERDICT\tDETAIL")
		for _, r := range rep.Resources {
			name := r.Name
			if r.Cluster != "" {
				name += " (" + r.Cluster + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Kind, name, r.Verdict, r.Detail)
		}
		tw.Flush()
	}

	s := rep.Summary
	fmt.Fprintf(w, "\n%d to create, %d to update, %d in sync, %d drifted, %d blocked, %d unfilled\n",
		s.Create, s.Update, s.InSync, s.Drifted, s.Blocked, s.Unfilled)

	for _, u := range rep.Unfilled {
		fmt.Fprintf(w, "%s — fill it in the console, or set its value_from_env variable and re-run\n", unfilledLine(u))
	}
}

func unfilledLine(u ReportUnfilled) string {
	switch u.Kind {
	case "stack_env":
		return fmt.Sprintf("unfilled: stack %s env %s", u.Stack, u.Name)
	case "stack_secret_file":
		return fmt.Sprintf("unfilled: stack %s secret file %s", u.Stack, u.Name)
	default:
		return fmt.Sprintf("unfilled: %s %s", u.Kind, u.Name)
	}
}

// reportExit turns a report into a process exit code.
//
//	plan:  0 everything in sync; 2 anything to do (create/update/drifted/blocked/
//	       unfilled) — so `daffa plan` in CI is a one-line drift check.
//	apply: 0 applied clean; 2 drifted or blocked remain — applied-but-diverged,
//	       which CI must notice even though the apply itself did not fail.
func reportExit(verb string, s ReportSummary) int {
	switch verb {
	case "plan":
		if s.Create+s.Update+s.Drifted+s.Blocked+s.Unfilled > 0 {
			return 2
		}
	default:
		if s.Drifted+s.Blocked > 0 {
			return 2
		}
	}
	return 0
}

// deployWalk deploys the manifest's stacks in document order, each to completion
// before the next. A stack with unfilled secret slots stops the walk BEFORE its
// deploy — a service booted with an empty secret fails somewhere much less legible
// than here.
func deployWalk(ctx context.Context, client *http.Client, o ManifestOptions, m *manifest.Manifest, rep *Report) error {
	for _, st := range m.Stacks {
		cluster := st.Cluster
		if cluster == "" {
			cluster = m.Cluster
		}
		if slots := unfilledForStack(rep, st.Name, cluster); len(slots) > 0 {
			names := make([]string, len(slots))
			for i, u := range slots {
				names[i] = u.Name
			}
			return fmt.Errorf("apply: not deploying stack %q — it has unfilled secrets (%s); fill them in the console and re-run",
				st.Name, strings.Join(names, ", "))
		}
		id := stackID(rep, st.Name, cluster)
		if id == "" {
			return fmt.Errorf("apply: the report carries no id for stack %q — was it blocked?", st.Name)
		}

		fmt.Fprintf(o.Log, "deploying %s… ", st.Name)
		start := time.Now()
		if err := deployStack(ctx, client, o, id); err != nil {
			fmt.Fprintln(o.Log, "failed")
			return err
		}
		fmt.Fprintf(o.Log, "ok (%s)\n", time.Since(start).Round(time.Second))
	}
	return nil
}

// unfilledForStack matches on (cluster, name) when the entry carries a cluster,
// so a slot on one cluster's stack never blocks deploying its same-named twin on
// another. Entries without a cluster fall back to name-only, which over-blocks
// rather than under-blocks.
func unfilledForStack(rep *Report, name, cluster string) []ReportUnfilled {
	var out []ReportUnfilled
	for _, u := range rep.Unfilled {
		if u.Kind != "stack_env" && u.Kind != "stack_secret_file" {
			continue
		}
		if u.Stack == name && (u.Cluster == "" || u.Cluster == cluster) {
			out = append(out, u)
		}
	}
	return out
}

// stackID resolves a stack's id from the report. Cluster breaks the tie when the
// same stack name exists on two clusters; a lone name match is enough otherwise.
func stackID(rep *Report, name, cluster string) string {
	var fallback string
	for _, r := range rep.Resources {
		if r.Kind != string(manifest.KindStack) || r.Name != name || r.ID == "" {
			continue
		}
		if r.Cluster == cluster {
			return r.ID
		}
		if fallback == "" {
			fallback = r.ID
		}
	}
	return fallback
}

func deployStack(ctx context.Context, client *http.Client, o ManifestOptions, stackID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Server+"/api/stacks/"+stackID+"/up", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "daffa-cli")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("apply: starting the deploy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("apply: starting the deploy: %s", apiError(resp))
	}

	var started struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		return fmt.Errorf("apply: reading the deploy response: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.PollInterval):
		}

		d, err := deploymentStatus(ctx, client, o, started.DeploymentID)
		if err != nil {
			return err
		}
		switch d.Status {
		case "running":
			continue
		case "ok":
			return nil
		default: // failed, cancelled
			if tail := logTail(d.Log, 20); tail != "" {
				fmt.Fprintln(o.Log, "\n--- deploy log ---\n"+tail)
			}
			return fmt.Errorf("apply: the deployment %s — see the console for the full log", d.Status)
		}
	}
}

func deploymentStatus(ctx context.Context, client *http.Client, o ManifestOptions, id string) (struct {
	Status string `json:"status"`
	Log    string `json:"log"`
}, error) {
	var d struct {
		Status string `json:"status"`
		Log    string `json:"log"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.Server+"/api/deployments/"+id, nil)
	if err != nil {
		return d, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return d, fmt.Errorf("apply: polling the deployment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d, fmt.Errorf("apply: polling the deployment: %s", apiError(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return d, fmt.Errorf("apply: reading the deployment: %w", err)
	}
	return d, nil
}

func logTail(log string, lines int) string {
	log = strings.TrimRight(log, "\n")
	if log == "" {
		return ""
	}
	all := strings.Split(log, "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}
