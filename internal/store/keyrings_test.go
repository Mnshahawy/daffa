package store

import (
	"context"
	"testing"
)

// Keyrings are rotatable: a keyring is a stable name over an append-only set of versions, so
// "rotate" means "new data uses the new key" rather than "all old data is now unreadable". This
// exercises that end to end — rotation demotes rather than deletes, retiring the active version
// is refused, and deliveries filter by scope and cascade with their host.
func TestKeyringRotationAndDeliveries(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		prod := &Environment{Name: "prod"}
		staging := &Environment{Name: "staging"}
		for _, e := range []*Environment{prod, staging} {
			if err := s.CreateEnvironment(ctx, e); err != nil {
				t.Fatal(err)
			}
		}

		// The first rotation seeds the active version, the second demotes it — never deletes it,
		// which is the entire point of versioning.
		kr := &Keyring{Name: "orders-db", RotateDays: 30}
		if err := s.CreateKeyring(ctx, kr); err != nil {
			t.Fatal(err)
		}
		v1, err := s.RotateKeyring(ctx, kr.ID, "sealed-v1")
		if err != nil {
			t.Fatal(err)
		}
		v2, err := s.RotateKeyring(ctx, kr.ID, "sealed-v2")
		if err != nil {
			t.Fatal(err)
		}
		vs, err := s.KeyringVersions(ctx, kr.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(vs) != 2 {
			t.Fatalf("got %d versions after two rotations; want 2 — rotation must append, not replace", len(vs))
		}
		states := map[string]string{}
		for _, v := range vs {
			states[v.ID] = v.State
		}
		if states[v2.ID] != KeyringVersionActive || states[v1.ID] != KeyringVersionDecryptOnly {
			t.Fatalf("after rotation: v1=%s v2=%s; want v1 demoted to decrypt_only and v2 active", states[v1.ID], states[v2.ID])
		}

		// Retiring the ACTIVE version is refused in the WHERE clause itself, so no
		// handler race can strand the keyring with nothing to encrypt with.
		if ok, err := s.RetireKeyringVersion(ctx, v2.ID); err != nil || ok {
			t.Fatalf("retiring the active version = (%v, %v); want a refusal", ok, err)
		}
		if ok, err := s.RetireKeyringVersion(ctx, v1.ID); err != nil || !ok {
			t.Fatalf("retiring a decrypt_only version = (%v, %v); want it to succeed", ok, err)
		}

		// Deliveries: scoped list filtering, the InUse count, and the env cascade.
		d := &KeyringDelivery{KeyringID: kr.ID, EnvID: staging.ID}
		if err := s.CreateKeyringDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
		if d.Volume != "daffa-keys" {
			t.Errorf("delivery volume did not default: %q", d.Volume)
		}
		if got, _ := s.ListKeyringDeliveries(ctx, false, []string{staging.ID}); len(got) != 1 {
			t.Errorf("a staging-scoped reader saw %d deliveries; want 1", len(got))
		}
		if got, _ := s.ListKeyringDeliveries(ctx, false, []string{prod.ID}); len(got) != 0 {
			t.Errorf("a prod-scoped reader saw %d staging deliveries; want 0", len(got))
		}
		// No hosts at all ⇒ nothing — the empty-IN-list trap.
		if got, _ := s.ListKeyringDeliveries(ctx, false, nil); len(got) != 0 {
			t.Errorf("a reader with no hosts saw %d deliveries; want 0", len(got))
		}
		if n, err := s.KeyringInUse(ctx, kr.ID); err != nil || n != 1 {
			t.Fatalf("KeyringInUse = (%d, %v); want 1 — the delete handler leans on this count", n, err)
		}

		// Removing the environment removes its deliveries (a delivery onto a deleted host
		// means nothing) but never the keyring or its versions.
		if _, err := s.exec(ctx, `DELETE FROM environments WHERE id = ?`, staging.ID); err != nil {
			t.Fatal(err)
		}
		if n, _ := s.KeyringInUse(ctx, kr.ID); n != 0 {
			t.Errorf("deliveries survived their environment's deletion: %d", n)
		}
		if _, err := s.KeyringByID(ctx, kr.ID); err != nil {
			t.Errorf("the keyring must outlive an environment that mounted it: %v", err)
		}

		// Deleting the keyring takes its versions with it — the rows are only an audit
		// trail of material that is now gone anyway.
		if err := s.DeleteKeyring(ctx, kr.ID); err != nil {
			t.Fatal(err)
		}
		if vs, _ := s.KeyringVersions(ctx, kr.ID); len(vs) != 0 {
			t.Errorf("versions survived their keyring's deletion: %d rows", len(vs))
		}
	})
}
