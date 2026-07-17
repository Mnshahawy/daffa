package store

import (
	"context"
	"testing"
)

// "Never deployed" and "the deploy failed" are different facts, and the difference is the one
// an operator actually needs.
//
// The case that produced this test, reproduced against a real daemon: an inline compose stack
// whose port was already taken. Compose CREATED the container and then failed to start it, so
// `up` exited 1 and Daffa — which records a deploy only on a clean up — recorded nothing. The
// operator then hit Restart, which started the container that compose had already made, and the
// stack came up and served traffic. Daffa went on calling it "never deployed", which was both
// wrong-sounding and useless: the thing to do was deploy again, and nothing said so.
func TestAFailedDeployIsNotTheSameAsNoDeploy(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		st := &Stack{
			EnvID: env, Name: "nginx-test", SourceKind: "inline",
			InlineYAML: "services:\n  web:\n    image: nginx:alpine\n",
		}
		if err := s.CreateStack(ctx, st); err != nil {
			t.Fatal(err)
		}

		// Nobody has tried yet.
		got := mustStack(t, s, st.ID)
		if got.LastDeployStatus != "" || !got.DeployedAt.IsZero() {
			t.Fatalf("a brand new stack reports a deploy: %q / %v",
				got.LastDeployStatus, got.DeployedAt)
		}

		// A deploy that FAILS. The port was taken; compose made the container and could not
		// start it.
		run := &Deployment{StackID: st.ID, Action: "up", BundleHash: "hash1"}
		if err := s.ClaimDeployment(ctx, run); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishDeployment(ctx, run.ID, 1, "Bind for 0.0.0.0:8081 failed: port is already allocated", false); err != nil {
			t.Fatal(err)
		}

		got = mustStack(t, s, st.ID)
		if got.LastDeployStatus != "failed" {
			t.Errorf("after a failed deploy the stack reports %q; want \"failed\"", got.LastDeployStatus)
		}
		if !got.DeployedAt.IsZero() {
			t.Error("a FAILED deploy marked the stack deployed — the whole point of the hash is " +
				"that it says what is actually live")
		}

		// The operator hits RESTART, which starts the container compose already created. It
		// works — but a restart does not apply a bundle, so it must not claim a deploy.
		rr := &Deployment{StackID: st.ID, Action: "restart"}
		if err := s.ClaimDeployment(ctx, rr); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishDeployment(ctx, rr.ID, 0, "", false); err != nil {
			t.Fatal(err)
		}

		got = mustStack(t, s, st.ID)
		if got.LastDeployStatus != "failed" {
			t.Errorf("a successful RESTART overwrote the last DEPLOY's outcome (%q). The two "+
				"are different questions: the deploy is still the thing that failed, and "+
				"deploying again is still what needs doing.", got.LastDeployStatus)
		}

		// And a real deploy clears it.
		good := &Deployment{StackID: st.ID, Action: "up", BundleHash: "hash2"}
		if err := s.ClaimDeployment(ctx, good); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishDeployment(ctx, good.ID, 0, "Started", false); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkStackDeployed(ctx, st.ID, "hash2", "abc1234"); err != nil {
			t.Fatal(err)
		}

		got = mustStack(t, s, st.ID)
		if got.LastDeployStatus != "ok" || got.DeployedAt.IsZero() {
			t.Errorf("after a good deploy: status=%q deployed=%v", got.LastDeployStatus, got.DeployedAt)
		}

		// The LIST carries it too — that is where "never deployed" is actually rendered.
		list, err := s.ListStacks(ctx, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].LastDeployStatus != "ok" {
			t.Errorf("the stack list does not carry the deploy outcome: %+v", list)
		}
	})
}

func mustStack(t *testing.T, s *Store, id string) *Stack {
	t.Helper()
	got, err := s.StackByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A stack secret is the file-shaped twin of an env var: sealed material delivered as a file
// the deploy writes into the bundle. This covers the wholesale-replace semantics (a save is the
// whole set, so a name dropped from it is deleted) and the cascade that takes a deleted stack's
// secrets with it.
func TestStackSecretsReplaceWholesaleAndCascade(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)
		st := &Stack{EnvID: env, Name: "web", Engine: "compose", SourceKind: "inline",
			InlineYAML: "services: {}"}
		if err := s.CreateStack(ctx, st); err != nil {
			t.Fatal(err)
		}

		// A fresh stack starts with no secrets.
		if secs, err := s.StackSecrets(ctx, st.ID); err != nil || len(secs) != 0 {
			t.Fatalf("a new stack must start with no secrets: %v, %v", secs, err)
		}

		// Replace-wholesale drops what is no longer in the set.
		if err := s.SetStackSecrets(ctx, st.ID, []StackSecret{
			{Name: "db_password", ContentEnc: "sealed-a"},
			{Name: "tls_key", ContentEnc: "sealed-b"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetStackSecrets(ctx, st.ID, []StackSecret{{Name: "db_password", ContentEnc: "sealed-a2"}}); err != nil {
			t.Fatal(err)
		}
		got, err := s.StackSecrets(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "db_password" || got[0].ContentEnc != "sealed-a2" {
			t.Fatalf("wholesale replace is wrong: %+v", got)
		}

		// ON DELETE CASCADE: deleting the stack takes its secrets with it.
		if err := s.DeleteStack(ctx, st.ID); err != nil {
			t.Fatal(err)
		}
		if after, err := s.StackSecrets(ctx, st.ID); err != nil || len(after) != 0 {
			t.Errorf("deleting a stack must cascade to its secrets: %v, %v", after, err)
		}
	})
}
