package store

import (
	"context"
	"errors"
	"testing"
)

// The SSH-key store keeps public material in the clear and the private half sealed by the
// caller. This exercises the round-trip and the name-uniqueness the delete/create guards lean on.
func TestSSHKeyStore(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		key := &SSHKey{
			Name: "prod-fleet", Algo: SSHKeyEd25519,
			PublicKey: "ssh-ed25519 AAAA… daffa:prod-fleet", Fingerprint: "SHA256:abc",
			PrivateKeyEnc: "sealed-private",
		}
		if err := s.CreateSSHKey(ctx, key); err != nil {
			t.Fatal(err)
		}
		if key.ID == "" || key.ID[:7] != "sshkey_" {
			t.Fatalf("id %q is not sshkey_-prefixed", key.ID)
		}

		got, err := s.SSHKeyByID(ctx, key.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Fingerprint != "SHA256:abc" || got.PrivateKeyEnc != "sealed-private" {
			t.Fatalf("round-trip lost fields: %+v", got)
		}

		// Nothing references a key yet, so it is never in use — the guard is wired for a later phase.
		if n, err := s.SSHKeyInUse(ctx, key.ID); err != nil || n != 0 {
			t.Fatalf("in-use = (%d, %v); want (0, nil)", n, err)
		}

		// Names are unique: two keys called the same thing is a switcher/list ambiguity.
		dup := &SSHKey{Name: "prod-fleet", Algo: SSHKeyRSA, PublicKey: "x", Fingerprint: "y", PrivateKeyEnc: "z"}
		if err := s.CreateSSHKey(ctx, dup); err == nil {
			t.Fatal("creating a second key with a taken name should fail")
		}

		if keys, err := s.ListSSHKeys(ctx); err != nil || len(keys) != 1 {
			t.Fatalf("list = (%d keys, %v); want (1, nil)", len(keys), err)
		}

		if err := s.DeleteSSHKey(ctx, key.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SSHKeyByID(ctx, key.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete, lookup err = %v; want ErrNotFound", err)
		}
	})
}
