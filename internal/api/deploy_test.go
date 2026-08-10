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

// A deployment's VERDICT must survive the deadline that produced it.
//
// This is the bug that made a stack undeployable and took the console down with it. A swarm
// `stack deploy` that could never converge ran until the 20-minute run bound expired; the
// pipeline then recorded the timeout using the very context that had just timed out, the write
// failed with "context deadline exceeded", and the row stayed `running` forever. Downstream:
// the stack's deploy claim was never released, so every later deploy was refused; and the log
// endpoint went on treating the row as live, replaying the whole 25,000-line runner log to
// every browser that reconnected until the tab died.
//
// So the assertion is deliberately brutal — an ALREADY-DEAD context, which is the exact state
// every timeout path arrives in. If a future refactor threads the run's context into one of
// these writes again, this fails.
func TestADeploymentIsRecordedEvenWhenItsContextIsAlreadyDead(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.DiscardHandler)
	s := &Server{store: st, pool: dockerx.NewPool(), notify: notify.New(st, fakeSealer{}, log)}

	env := &store.Environment{Name: "prod"}
	if err := st.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}

	// dead is what a run context looks like the moment after it times out.
	dead, cancel := context.WithCancel(ctx)
	cancel()

	for _, tc := range []struct {
		name   string
		stack  string
		finish func(*store.Stack, *store.Deployment)
		want   string
	}{
		{
			name:  "the funnel every completed deploy ends in",
			stack: "finish-deploy",
			finish: func(stack *store.Stack, dep *store.Deployment) {
				s.finishDeploy(dead, dep, stack, 1, "the runner could not be waited on", false)
			},
			want: store.DeployFailed,
		},
		{
			name:  "a deploy that fell over before the runner started",
			stack: "fail-deployment",
			finish: func(stack *store.Stack, dep *store.Deployment) {
				_ = s.failDeployment(dead, stack, dep, "the compose file will not parse")
			},
			want: store.DeployFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack := &store.Stack{
				EnvID: env.ID, Name: tc.stack, SourceKind: "inline",
				InlineYAML: "services:\n  app:\n    image: nginx:alpine\n",
			}
			if err := st.CreateStack(ctx, stack); err != nil {
				t.Fatal(err)
			}
			dep := &store.Deployment{StackID: stack.ID, Action: string(stacks.ActionUp), Engine: "compose"}
			if err := st.ClaimDeployment(ctx, dep); err != nil {
				t.Fatal(err)
			}

			tc.finish(stack, dep)

			got, err := st.DeploymentByID(ctx, dep.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status == store.DeployRunning {
				t.Fatalf("the deployment is STILL %q after being finished on a dead context.\n\n"+
					"A row stuck in `running` holds the stack's deploy claim forever and makes "+
					"the log endpoint replay a live stream that will never end. The verdict has "+
					"to be written on a context detached from the run's deadline — see "+
					"finishContext.", got.Status)
			}
			if got.Status != tc.want {
				t.Errorf("the deployment is %q; want %q", got.Status, tc.want)
			}

			// And the claim is gone with it: the whole cost of a stuck row is the next deploy.
			second := &store.Deployment{StackID: stack.ID, Action: string(stacks.ActionUp), Engine: "compose"}
			if err := st.ClaimDeployment(ctx, second); err != nil {
				t.Fatalf("the stack could not be deployed again: %v\n\n"+
					"The finished deployment never released its claim, so this stack is now "+
					"permanently undeployable.", err)
			}
		})
	}
}

type fakeSealer struct{}

func (fakeSealer) Open(s string) (string, error) { return s, nil }
