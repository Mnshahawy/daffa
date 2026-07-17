package api

import (
	"testing"

	"github.com/Mnshahawy/daffa/internal/stacks"
)

// Adoption must leave the stack reading as in-sync, not "never deployed" — so the hash a
// deploy would compute right now has to equal the DeployedHash adoption stamped. This is the
// whole correctness claim; if it fails the console shows permanent, phantom drift.
func TestAdoptStackReadsClean(t *testing.T) {
	s, ctx := certServer(t)

	env, _, err := s.store.UpsertLocalEnvironment(ctx, "Local", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}

	yaml := "name: daffa\nservices:\n  app:\n    image: ${DAFFA_IMAGE}\n"
	kv := []EnvKV{
		{Key: "DAFFA_IMAGE", Value: "ghcr.io/x/daffa:1"},
		{Key: "POSTGRES_PASSWORD", Value: "s3cr3t-value", Secret: true},
	}

	res, err := s.AdoptStack(ctx, AdoptStackOptions{Name: "daffa", EnvID: env.ID, ComposeYAML: yaml, Env: kv})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Fatal("first adopt should create the stack")
	}

	st, err := s.store.StackByID(ctx, res.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "daffa" || st.SourceKind != "inline" || st.InlineYAML != yaml {
		t.Fatalf("adopted stack looks wrong: %+v", st)
	}
	if st.DeployedHash == "" {
		t.Fatal("DeployedHash was not set — the stack would read as never deployed")
	}

	// Recompute the hash exactly as buildBundle would (unseal env → Build over the original
	// YAML). It must match, i.e. no drift.
	envPlain, err := s.stackEnvPlain(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := stacks.Build(st.InlineYAML, envPlain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Hash != st.DeployedHash {
		t.Fatalf("phantom drift: DeployedHash %q != freshly computed %q", st.DeployedHash, b.Hash)
	}

	// The secret flag survives the round-trip so the UI hides the password.
	stored, err := s.store.StackEnv(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pgSecret, sawImage bool
	for _, e := range stored {
		switch e.Key {
		case "POSTGRES_PASSWORD":
			pgSecret = e.IsSecret
		case "DAFFA_IMAGE":
			sawImage = true
		}
	}
	if !pgSecret {
		t.Error("POSTGRES_PASSWORD should be stored as a secret")
	}
	if !sawImage {
		t.Error("DAFFA_IMAGE should have been captured")
	}

	// Idempotent: a second adopt updates in place rather than making a parallel stack.
	res2, err := s.AdoptStack(ctx, AdoptStackOptions{Name: "daffa", EnvID: env.ID, ComposeYAML: yaml, Env: kv})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created || res2.StackID != res.StackID {
		t.Errorf("re-adopt should reuse the same stack, got created=%v id=%s", res2.Created, res2.StackID)
	}
	all, err := s.store.ListStacks(ctx, false, []string{env.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("re-adopt created a duplicate: %d stacks", len(all))
	}
}
