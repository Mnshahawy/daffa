package store

import (
	"context"
	"errors"
	"testing"
)

func TestManifestApplyHistory(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		first := &ManifestApply{Name: "my-app", DocHash: "sha256:aaa",
			Document: "version: 1\nname: my-app\n", Report: `{"summary":{}}`,
			AppliedBy: "admin", DryRun: true}
		if err := s.CreateManifestApply(ctx, first); err != nil {
			t.Fatalf("CreateManifestApply: %v", err)
		}
		second := &ManifestApply{Name: "my-app", DocHash: "sha256:bbb",
			Document: "version: 1\nname: my-app\ncluster: prod\n", Report: `{"summary":{}}`,
			AppliedBy: "admin"}
		if err := s.CreateManifestApply(ctx, second); err != nil {
			t.Fatal(err)
		}

		got, err := s.ManifestApplyByID(ctx, first.ID)
		if err != nil {
			t.Fatalf("ManifestApplyByID: %v", err)
		}
		// The document must come back byte-identical: history exists to answer "was
		// THIS exact file applied?".
		if got.Document != first.Document || got.DocHash != first.DocHash {
			t.Errorf("round-trip changed the document: %+v", got)
		}
		if !got.DryRun || got.AppliedBy != "admin" || got.AppliedAt.IsZero() {
			t.Errorf("round-trip lost fields: %+v", got)
		}

		list, err := s.ListManifestApplies(ctx)
		if err != nil {
			t.Fatalf("ListManifestApplies: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("listed %d applies; want 2", len(list))
		}
		if list[0].ID != second.ID {
			t.Errorf("list is not newest-first: %s before %s", list[0].ID, list[1].ID)
		}

		if _, err := s.ManifestApplyByID(ctx, "man_missing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing apply: got %v, want ErrNotFound", err)
		}
	})
}

// The manifest reconciler resolves every declared name through these lookups; each one
// mirrors the unique index that makes the name an identity.
func TestManifestNameLookups(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		prod := &Environment{Name: "prod"}
		staging := &Environment{Name: "staging"}
		for _, e := range []*Environment{prod, staging} {
			if err := s.CreateEnvironment(ctx, e); err != nil {
				t.Fatal(err)
			}
		}

		// Stacks: unique per (env, name) — the same name on two hosts is two stacks.
		for _, envID := range []string{prod.ID, staging.ID} {
			st := &Stack{EnvID: envID, Name: "api", Engine: "compose", SourceKind: "inline", InlineYAML: "services: {}"}
			if err := s.CreateStack(ctx, st); err != nil {
				t.Fatal(err)
			}
		}
		st, err := s.StackByName(ctx, prod.ID, "api")
		if err != nil || st.EnvID != prod.ID {
			t.Fatalf("StackByName(prod, api) = %+v, %v", st, err)
		}
		if _, err := s.StackByName(ctx, prod.ID, "missing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing stack: got %v, want ErrNotFound", err)
		}

		ca := &CertAuthority{Name: "app-ca", Subject: "CN=App CA", CertPEM: "PEM"}
		if err := s.CreateCertAuthority(ctx, ca); err != nil {
			t.Fatal(err)
		}
		if got, err := s.CertAuthorityByName(ctx, "app-ca"); err != nil || got.ID != ca.ID {
			t.Fatalf("CertAuthorityByName = %v, %v", got, err)
		}
		if _, err := s.CertAuthorityByName(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing CA: got %v, want ErrNotFound", err)
		}

		// Certificates: a shared cert (no env) and an env-scoped one under the same
		// name are distinct rows, and envID "" must find the SHARED one — the same
		// COALESCE the unique index applies.
		shared := &Certificate{Name: "api", CAID: ca.ID, CertPEM: "PEM", KeyEnc: "sealed"}
		scoped := &Certificate{Name: "api", EnvID: prod.ID, CAID: ca.ID, CertPEM: "PEM", KeyEnc: "sealed"}
		for _, c := range []*Certificate{shared, scoped} {
			if err := s.CreateCertificate(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := s.CertificateByName(ctx, "", "api"); err != nil || got.ID != shared.ID {
			t.Fatalf("CertificateByName(shared) = %+v, %v; want the env-less row", got, err)
		}
		if got, err := s.CertificateByName(ctx, prod.ID, "api"); err != nil || got.ID != scoped.ID {
			t.Fatalf("CertificateByName(prod) = %+v, %v", got, err)
		}

		reg := &Registry{Name: "ghcr", URL: "ghcr.io", Username: "ci", PasswordEnc: "sealed"}
		if err := s.CreateRegistry(ctx, reg); err != nil {
			t.Fatal(err)
		}
		if got, err := s.RegistryByName(ctx, "ghcr"); err != nil || got.ID != reg.ID {
			t.Fatalf("RegistryByName = %v, %v", got, err)
		}
		if _, err := s.RegistryByName(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing registry: got %v, want ErrNotFound", err)
		}

		key := &SSHKey{Name: "deploy-key", Algo: SSHKeyEd25519, PublicKey: "ssh-ed25519 AAA", Fingerprint: "SHA256:x", PrivateKeyEnc: "sealed"}
		if err := s.CreateSSHKey(ctx, key); err != nil {
			t.Fatal(err)
		}
		if got, err := s.SSHKeyByName(ctx, "deploy-key"); err != nil || got.ID != key.ID {
			t.Fatalf("SSHKeyByName = %v, %v", got, err)
		}

		cred := &GitCredential{Name: "app-repo", Kind: GitSSH, SSHKeyID: key.ID}
		if err := s.CreateGitCredential(ctx, cred); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GitCredentialByName(ctx, "app-repo"); err != nil || got.ID != cred.ID {
			t.Fatalf("GitCredentialByName = %v, %v", got, err)
		}

		kr := &Keyring{Name: "app-secrets"}
		if err := s.CreateKeyring(ctx, kr); err != nil {
			t.Fatal(err)
		}
		if got, err := s.KeyringByName(ctx, "app-secrets"); err != nil || got.ID != kr.ID {
			t.Fatalf("KeyringByName = %v, %v", got, err)
		}

		// Keyring deliveries: several keyrings share a volume (the keyring name is the
		// filename prefix), so the lookup is keyed on all three of keyring, env, volume.
		kr2 := &Keyring{Name: "other-secrets"}
		if err := s.CreateKeyring(ctx, kr2); err != nil {
			t.Fatal(err)
		}
		d1 := &KeyringDelivery{KeyringID: kr.ID, EnvID: prod.ID, Volume: "app-keys"}
		d2 := &KeyringDelivery{KeyringID: kr2.ID, EnvID: prod.ID, Volume: "app-keys"}
		for _, d := range []*KeyringDelivery{d1, d2} {
			if err := s.CreateKeyringDelivery(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := s.KeyringDeliveryForVolume(ctx, kr.ID, prod.ID, "app-keys"); err != nil || got.ID != d1.ID {
			t.Fatalf("KeyringDeliveryForVolume(kr) = %v, %v", got, err)
		}
		if got, err := s.KeyringDeliveryForVolume(ctx, kr2.ID, prod.ID, "app-keys"); err != nil || got.ID != d2.ID {
			t.Fatalf("KeyringDeliveryForVolume(kr2) = %v, %v", got, err)
		}
		if _, err := s.KeyringDeliveryForVolume(ctx, kr.ID, staging.ID, "app-keys"); !errors.Is(err, ErrNotFound) {
			t.Errorf("delivery on the wrong env: got %v, want ErrNotFound", err)
		}
	})
}
