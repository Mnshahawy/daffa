package store

import (
	"context"
	"testing"
)

// A git credential is "in use" if ANYTHING references it — a stack OR a volume source. Counting
// only stacks let a credential used solely by a volume source pass the delete guard and then hit
// the raw FK, surfacing as a 500 instead of the friendly refusal the guard exists to give.
func TestGitCredentialInUseCountsVolumeSources(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		cred := &GitCredential{Name: "deploy-key", Kind: GitSSH, SSHKeyID: "sshkey_deploy"}
		if err := s.CreateGitCredential(ctx, cred); err != nil {
			t.Fatal(err)
		}

		// Nothing references it yet.
		if n, err := s.GitCredentialInUse(ctx, cred.ID); err != nil || n != 0 {
			t.Fatalf("in-use before any reference = (%d, %v); want (0, nil)", n, err)
		}

		// A volume source alone must make it in-use — the case the old stacks-only count missed.
		vs := &VolumeSource{
			EnvID: env, Volume: "assets", GitURL: "https://example.com/repo.git",
			GitRef: "main", GitCredentialID: cred.ID,
		}
		if err := s.CreateVolumeSource(ctx, vs); err != nil {
			t.Fatal(err)
		}
		if n, err := s.GitCredentialInUse(ctx, cred.ID); err != nil || n != 1 {
			t.Fatalf("in-use with one volume source = (%d, %v); want (1, nil)", n, err)
		}

		// Adding a stack that also uses it counts both dependents.
		st := &Stack{
			EnvID: env, Name: "web", SourceKind: "git",
			GitURL: "https://example.com/repo.git", GitCredentialID: cred.ID,
		}
		if err := s.CreateStack(ctx, st); err != nil {
			t.Fatal(err)
		}
		if n, err := s.GitCredentialInUse(ctx, cred.ID); err != nil || n != 2 {
			t.Fatalf("in-use with a stack and a volume source = (%d, %v); want (2, nil)", n, err)
		}
	})
}
