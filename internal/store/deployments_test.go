package store

import (
	"context"
	"testing"
	"time"
)

// A deploy somebody CANCELLED is not a deploy that FAILED.
//
// The daemon cannot tell them apart: a killed runner and a broken one both exit non-zero. Only
// the cancel flag can, and it is read in FinishDeployment because that is the one place every
// deployment ends. Get this wrong and every deliberate cancel is reported as a failure, emailed
// to the team, and counted against the stack — training everyone to ignore the alert that
// matters.
func TestACancelledDeployIsNotAFailedOne(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		st := oneStack(t, s)

		d := &Deployment{StackID: st, Action: "up"}
		if err := s.ClaimDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}

		flagged, err := s.RequestCancel(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !flagged {
			t.Fatal("a running deployment refused to be cancelled")
		}

		// The runner is killed, so it exits non-zero — exactly like a failure.
		status, err := s.FinishDeployment(ctx, d.ID, 137, "killed", false)
		if err != nil {
			t.Fatal(err)
		}
		if status != DeployCancelled {
			t.Errorf("a cancelled deploy was recorded as %q; want cancelled.\n\n"+
				"It exits non-zero because it was KILLED. Reporting somebody's own decision back "+
				"to them as a failure — and emailing the team about it — is how an alert channel "+
				"gets muted.", status)
		}

		got, err := s.DeploymentByID(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != DeployCancelled {
			t.Errorf("stored status is %q; want cancelled", got.Status)
		}
	})
}

// Cancelling something that already finished is not a no-op, it is a lie: the UI would say
// "cancelled" about a deploy that in fact completed and changed production.
func TestCancellingAFinishedDeployDoesNothing(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		st := oneStack(t, s)

		d := &Deployment{StackID: st, Action: "up"}
		if err := s.ClaimDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishDeployment(ctx, d.ID, 0, "done", false); err != nil {
			t.Fatal(err)
		}

		flagged, err := s.RequestCancel(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if flagged {
			t.Error("a deployment that had already finished reported itself cancelled")
		}

		got, _ := s.DeploymentByID(ctx, d.ID)
		if got.Status != DeployOK {
			t.Errorf("a finished deploy became %q after a late cancel; it succeeded and must stay "+
				"that way", got.Status)
		}
	})
}

// Retention keeps the last N per stack AND everything recent — a union, not an intersection.
// Either rule on its own throws away something somebody would miss.
func TestPruneKeepsRecentAndTheLastFew(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		st := oneStack(t, s)

		// Twelve old deployments, all well past the age cutoff.
		var ids []string
		for i := range 12 {
			d := &Deployment{StackID: st, Action: "up"}
			if err := s.ClaimDeployment(ctx, d); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishDeployment(ctx, d.ID, 0, "ok", false); err != nil {
				t.Fatal(err)
			}
			// Backdate: ClaimDeployment stamps now(), and the point of the test is age.
			old := ts(time.Now().Add(-time.Duration(200-i) * 24 * time.Hour))
			if _, err := s.exec(ctx, `UPDATE deployments SET started_at = ? WHERE id = ?`, old, d.ID); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, d.ID)
		}

		// Keep the newest 5, and nothing older than 90 days. All twelve are older than 90 days,
		// so exactly the 5 newest should survive on the count rule alone.
		if _, err := s.PruneDeployments(ctx, 5, 90*24*time.Hour); err != nil {
			t.Fatal(err)
		}

		left, err := s.ListDeployments(ctx, st, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 5 {
			t.Fatalf("pruning to keep 5 left %d", len(left))
		}
		// The survivors must be the NEWEST five, not any five.
		for _, d := range left {
			if !contains(ids[7:], d.ID) {
				t.Errorf("prune kept an older deployment (%s) over a newer one — the recent history "+
					"is the part anybody reads", d.ID)
			}
		}
	})
}

// A running deployment is the stack's LOCK. Pruning one would let a second deploy start on top of
// a live one, which is the exact race the claim exists to prevent.
func TestPruneNeverRemovesARunningDeployment(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		st := oneStack(t, s)

		d := &Deployment{StackID: st, Action: "up"}
		if err := s.ClaimDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
		// Make it look ancient — a deploy that has been running for a year is precisely the row a
		// naive age-based prune would eat.
		old := ts(time.Now().Add(-365 * 24 * time.Hour))
		if _, err := s.exec(ctx, `UPDATE deployments SET started_at = ? WHERE id = ?`, old, d.ID); err != nil {
			t.Fatal(err)
		}

		if _, err := s.PruneDeployments(ctx, 1, time.Hour); err != nil {
			t.Fatal(err)
		}

		if _, err := s.DeploymentByID(ctx, d.ID); err != nil {
			t.Fatalf("prune deleted a RUNNING deployment: %v.\n\n"+
				"That row is the stack's lock. Without it a second deploy can start on top of the "+
				"one still going.", err)
		}
	})
}

// The claim is the lock, and it is in the database rather than in a mutex because the thing it
// guards — a detached runner container — outlives the process.
func TestOneDeploymentAtATime(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		st := oneStack(t, s)

		first := &Deployment{StackID: st, Action: "up"}
		if err := s.ClaimDeployment(ctx, first); err != nil {
			t.Fatal(err)
		}

		second := &Deployment{StackID: st, Action: "up"}
		if err := s.ClaimDeployment(ctx, second); err != ErrRunInProgress {
			t.Fatalf("a second deploy claimed a stack that was already deploying: %v", err)
		}

		// And once the first is done, the stack is free again.
		if _, err := s.FinishDeployment(ctx, first.ID, 0, "", false); err != nil {
			t.Fatal(err)
		}
		if err := s.ClaimDeployment(ctx, second); err != nil {
			t.Fatalf("the stack stayed locked after its deploy finished: %v", err)
		}
	})
}

// The cross-stack feed is scoped: a deployment on a host you hold nothing on is not yours to see.
// Its stack's name alone usually names a customer or a service.
func TestTheFeedOnlyShowsHostsYouCanSee(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		mine := oneHost(t, s)
		theirs := namedHost(t, s, "other-host")

		for _, env := range []string{mine, theirs} {
			st := &Stack{EnvID: env, Name: "web-" + env[:4], SourceKind: "inline", InlineYAML: "x"}
			if err := s.CreateStack(ctx, st); err != nil {
				t.Fatal(err)
			}
			d := &Deployment{StackID: st.ID, Action: "up"}
			if err := s.ClaimDeployment(ctx, d); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishDeployment(ctx, d.ID, 0, "ok", false); err != nil {
				t.Fatal(err)
			}
		}

		scoped, err := s.RecentDeployments(ctx, false, []string{mine}, DeploymentFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(scoped) != 1 {
			t.Fatalf("a feed scoped to one host returned %d deployments; want 1", len(scoped))
		}

		global, err := s.RecentDeployments(ctx, true, nil, DeploymentFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(global) != 2 {
			t.Fatalf("the fleet-wide feed returned %d deployments; want 2", len(global))
		}

		// Nobody at all: not an error, and not everything either.
		none, err := s.RecentDeployments(ctx, false, nil, DeploymentFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(none) != 0 {
			t.Errorf("a user granted nothing anywhere saw %d deployments", len(none))
		}
	})
}

// namedHost is oneHost for a test that needs a second one: environment names are unique, so
// oneHost cannot be called twice in the same store.
func namedHost(t *testing.T, s *Store, name string) string {
	t.Helper()
	e := &Environment{Name: name}
	if err := s.CreateEnvironment(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e.ID
}

func oneStack(t *testing.T, s *Store) string {
	t.Helper()
	st := &Stack{
		EnvID: oneHost(t, s), Name: "web", SourceKind: "inline",
		InlineYAML: "services:\n  app:\n    image: nginx\n",
	}
	if err := s.CreateStack(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	return st.ID
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
