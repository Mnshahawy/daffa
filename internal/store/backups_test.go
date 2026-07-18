package store

import (
	"context"
	"testing"
)

// A volume job carries its exclude list through the store unchanged — it is stored on the row
// (like stop_containers), not in a side table, so this is a plain column round-trip. The empty
// case matters as much as the set one: the default is "snapshot everything".
func TestVolumeBackupJobRoundTripsExcludePaths(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		env := &Environment{Name: "prod"}
		if err := s.CreateEnvironment(ctx, env); err != nil {
			t.Fatal(err)
		}
		target := &StorageTarget{Name: "r2", Endpoint: "https://r2.example.com", Bucket: "backups",
			KeyID: "k", SecretEnc: "sealed"}
		if err := s.CreateStorageTarget(ctx, target); err != nil {
			t.Fatal(err)
		}

		withExcludes := &BackupJob{EnvID: env.ID, Name: "with excludes", Engine: "volume",
			Volume: "forgejo-data", StorageID: target.ID, Encryption: "none",
			ExcludePaths: "cache\ntmp/sessions"}
		none := &BackupJob{EnvID: env.ID, Name: "no excludes", Engine: "volume",
			Volume: "other-data", StorageID: target.ID, Encryption: "none"}
		for _, j := range []*BackupJob{withExcludes, none} {
			if err := s.CreateBackupJob(ctx, j); err != nil {
				t.Fatal(err)
			}
		}

		got, err := s.BackupJobByID(ctx, withExcludes.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExcludePaths != "cache\ntmp/sessions" {
			t.Errorf("ExcludePaths = %q; want %q", got.ExcludePaths, "cache\ntmp/sessions")
		}
		gotNone, err := s.BackupJobByID(ctx, none.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotNone.ExcludePaths != "" {
			t.Errorf("ExcludePaths on a job without excludes = %q; want empty", gotNone.ExcludePaths)
		}
	})
}

// A backup job encrypts to NAMED keys, resolved to age recipients at run time — the seam that
// keeps the backup pipeline ignorant of key management. This covers that resolution, the
// deduplication of a recipient shared by two jobs, and the InUse count the delete handler leans
// on to refuse dropping a recipient out from under a job that still encrypts to it.
func TestBackupJobKeysResolveToRecipients(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		env := &Environment{Name: "prod"}
		if err := s.CreateEnvironment(ctx, env); err != nil {
			t.Fatal(err)
		}
		target := &StorageTarget{Name: "r2", Endpoint: "https://r2.example.com", Bucket: "backups",
			KeyID: "k", SecretEnc: "sealed"}
		if err := s.CreateStorageTarget(ctx, target); err != nil {
			t.Fatal(err)
		}

		const recA = "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqaaaaa"
		const recB = "age1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzbbbbb"
		keyA := &EncryptionKey{Name: "key a", Recipient: recA}
		keyB := &EncryptionKey{Name: "key b", Recipient: recB}
		for _, k := range []*EncryptionKey{keyA, keyB} {
			if err := s.CreateEncryptionKey(ctx, k); err != nil {
				t.Fatal(err)
			}
		}

		// Two jobs: one encrypting to both keys, one sharing keyA. The shared key must report
		// itself in use by both.
		two := &BackupJob{EnvID: env.ID, Name: "two keys", Container: "db-1", Engine: "postgres",
			StorageID: target.ID, Encryption: "age", KeyIDs: []string{keyA.ID, keyB.ID}}
		shared := &BackupJob{EnvID: env.ID, Name: "shared key", Container: "db-2", Engine: "postgres",
			StorageID: target.ID, Encryption: "age", KeyIDs: []string{keyA.ID}}
		for _, j := range []*BackupJob{two, shared} {
			if err := s.CreateBackupJob(ctx, j); err != nil {
				t.Fatal(err)
			}
		}

		// Each job resolves to exactly the recipients it was given.
		for id, want := range map[string][]string{
			two.ID:    {recA, recB},
			shared.ID: {recA},
		} {
			got, err := s.JobRecipients(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("job %s encrypts to %v; want %v", id, got, want)
			}
			for _, rec := range want {
				found := false
				for _, g := range got {
					found = found || g == rec
				}
				if !found {
					t.Errorf("job %s is missing recipient %s", id, rec)
				}
			}
		}

		// The loaded job carries its key ids back.
		got, err := s.BackupJobByID(ctx, two.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.KeyIDs) != 2 {
			t.Errorf("BackupJobByID returned %d key ids; want 2", len(got.KeyIDs))
		}

		// keyA is used by BOTH jobs; keyB by one. The count is what the delete handler refuses on.
		if n, err := s.EncryptionKeyInUse(ctx, keyA.ID); err != nil || n != 2 {
			t.Errorf("EncryptionKeyInUse(keyA) = (%d, %v); want 2", n, err)
		}
		if n, err := s.EncryptionKeyInUse(ctx, keyB.ID); err != nil || n != 1 {
			t.Errorf("EncryptionKeyInUse(keyB) = (%d, %v); want 1", n, err)
		}
	})
}
