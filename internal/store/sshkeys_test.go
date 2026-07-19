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

// An SSH cluster is an environment plus a node that carries the connection config, and the key it
// dials with must count as in-use so it cannot be deleted out from under a live cluster.
func TestSSHClusterNodeAndKeyInUse(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		key := &SSHKey{Name: "fleet", Algo: SSHKeyEd25519, PublicKey: "ssh-ed25519 AAAA fleet",
			Fingerprint: "SHA256:z", PrivateKeyEnc: "sealed"}
		if err := s.CreateSSHKey(ctx, key); err != nil {
			t.Fatal(err)
		}

		env, node, err := s.CreateSSHNode(ctx, "prod-eu", &Node{
			SSHHost: "10.0.0.9", SSHPort: 22, SSHUser: "daffa", SSHKeyID: key.ID,
			SSHEndpoint: "unix:///var/run/docker.sock",
		})
		if err != nil {
			t.Fatal(err)
		}
		if node.Kind != "ssh" || node.EnvID != env.ID {
			t.Fatalf("ssh node not wired to its env: %+v", node)
		}

		// Round-trip the connection config through the DB.
		got, err := s.NodeByID(ctx, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.SSHHost != "10.0.0.9" || got.SSHUser != "daffa" || got.SSHKeyID != key.ID || got.SSHPort != 22 {
			t.Fatalf("ssh config did not round-trip: %+v", got)
		}

		// SSHNodes lists it; the pool's boot loop relies on this.
		if ns, err := s.SSHNodes(ctx); err != nil || len(ns) != 1 {
			t.Fatalf("SSHNodes = (%d, %v); want (1, nil)", len(ns), err)
		}

		// The key is now in use, so deleting it must be refused by the guard.
		if n, err := s.SSHKeyInUse(ctx, key.ID); err != nil || n != 1 {
			t.Fatalf("SSHKeyInUse with one cluster = (%d, %v); want (1, nil)", n, err)
		}

		// Pin a host key (TOFU), then remove the cluster — env and node go together.
		if err := s.SetNodeHostKey(ctx, node.ID, "ssh-ed25519 AAAAHOST"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteEnvironment(ctx, env.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NodeByID(ctx, node.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("node should cascade-delete with its env; got err %v", err)
		}
		if n, err := s.SSHKeyInUse(ctx, key.ID); err != nil || n != 0 {
			t.Fatalf("SSHKeyInUse after cluster removed = (%d, %v); want (0, nil)", n, err)
		}
	})
}
