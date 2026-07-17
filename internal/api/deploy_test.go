package api

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// EVERY deploy attempt is a deployment, and every deployment has a log.
//
// This was not true, and the way it failed was invisible. A deploy that fell over during
// PREPARATION — a compose file that would not parse, a repo that would not clone, a host that
// was not connected — never got as far as claiming a deployment. So it left nothing in the history to
// click on, nothing to read afterwards, and the error existed only as a red line on the page
// that a refresh wiped away. You could watch a deploy fail and then have no way to find out why.
//
// It also never notified. An auto-deploy that a webhook started at 2am, against a compose file
// somebody had broken that afternoon, failed in total silence — which is exactly the deploy you
// most needed to be told about.
func TestEveryDeployAttemptIsRecorded(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.DiscardHandler)
	s := &Server{
		store:  st,
		pool:   dockerx.NewPool(), // empty: no host is connected
		notify: notify.New(st, fakeSealer{}, log),
	}

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

	// The host is not connected — the deploy cannot even begin. This is the earliest way a
	// deploy can fail, and therefore the one most likely to leave no trace.
	if _, err := s.deploy(ctx, stack, stacks.ActionUp, store.TriggerManual, "", nil); err == nil {
		t.Fatal("deploying to a host that is not connected succeeded")
	}

	runs, err := st.ListDeployments(ctx, stack.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("a deploy that failed before it started left %d deployments; want 1.\n\n"+
			"With no run there is nothing in the Deployments list to click on and no log to "+
			"read: the operator watches it fail and then cannot find out why.", len(runs))
	}

	r := runs[0]
	if r.Status != store.DeployFailed {
		t.Errorf("the deployment is %q; want failed", r.Status)
	}
	if r.Log == "" {
		t.Error("the failed deployment has an EMPTY log — the reason it failed is the only thing " +
			"anybody wants from it")
	}
	if r.TriggerKind != store.TriggerManual {
		t.Errorf("the deployment records trigger %q; want manual", r.TriggerKind)
	}
	if !strings.Contains(strings.ToLower(r.Log), "not connected") {
		t.Errorf("the log does not say why it failed: %q", r.Log)
	}

	// And the claim is released: a stack must not be locked out of deploying forever because
	// one attempt died early.
	if _, err := s.deploy(ctx, stack, stacks.ActionUp, store.TriggerManual, "", nil); err != nil {
		if strings.Contains(err.Error(), "in progress") {
			t.Fatal("a deploy that failed before it started left the stack claimed — it can " +
				"never be deployed again")
		}
	}
}

type fakeSealer struct{}

func (fakeSealer) Open(s string) (string, error) { return s, nil }
