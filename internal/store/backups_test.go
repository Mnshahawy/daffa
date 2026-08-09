package store

import (
	"context"
	"os"
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

// Changing a schedule must move ONLY the schedule. The destination, the sealed password and the
// encryption keys are what the job IS — an "update" that reset any of them would turn a routine
// "run it at 4 instead of 3" into a job that dumps to the wrong place, or one nobody can decrypt.
// Clearing the schedule back to manual-only is a legitimate edit, not an empty-means-unchanged.
func TestSetBackupJobScheduleTouchesNothingElse(t *testing.T) {
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
		key := &EncryptionKey{Name: "key a",
			Recipient: "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqaaaaa"}
		if err := s.CreateEncryptionKey(ctx, key); err != nil {
			t.Fatal(err)
		}

		job := &BackupJob{EnvID: env.ID, Name: "nightly", Container: "db-1", Engine: "postgres",
			DBUser: "postgres", DBPasswordEnc: "sealed", Schedule: "0 3 * * *",
			StorageID: target.ID, Prefix: "prod/postgres", Encryption: "age",
			KeyIDs: []string{key.ID}, Enabled: true}
		if err := s.CreateBackupJob(ctx, job); err != nil {
			t.Fatal(err)
		}

		if err := s.SetBackupJobSchedule(ctx, job.ID, "0 4 * * *"); err != nil {
			t.Fatal(err)
		}
		got, err := s.BackupJobByID(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Schedule != "0 4 * * *" {
			t.Errorf("Schedule = %q; want %q", got.Schedule, "0 4 * * *")
		}
		if got.Container != "db-1" || got.DBPasswordEnc != "sealed" || got.StorageID != target.ID ||
			got.Prefix != "prod/postgres" || got.Encryption != "age" || !got.Enabled {
			t.Errorf("changing the schedule disturbed the job: %+v", got)
		}
		if len(got.KeyIDs) != 1 || got.KeyIDs[0] != key.ID {
			t.Errorf("KeyIDs = %v; want [%s]", got.KeyIDs, key.ID)
		}

		// Empty means manual only here, exactly as it does at creation.
		if err := s.SetBackupJobSchedule(ctx, job.ID, ""); err != nil {
			t.Fatal(err)
		}
		if got, err = s.BackupJobByID(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
		if got.Schedule != "" {
			t.Errorf("Schedule after clearing = %q; want empty", got.Schedule)
		}
	})
}

// A backup bigger than 2GiB has to survive its own run record. backup_runs.bytes was
// INTEGER from 0001 — 64-bit on SQLite, a SIGNED 32-bit int4 on Postgres — while the Go
// field holding the size of the artifact just uploaded is an int64. On Postgres every
// backup past the int4 ceiling therefore failed its FinishBackupRun UPDATE *after* the
// upload succeeded: the object sat in the bucket while the run row still read "running".
// Migration 0017 widens the column. Same trap as role_caps.mask and env_cleanup_runs.freed.
func TestABackupLargerThanInt4SurvivesTheDatabase(t *testing.T) {
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
		job := &BackupJob{EnvID: env.ID, Name: "postgres", Engine: "postgres", Container: "db-1",
			StorageID: target.ID, Encryption: "none"}
		if err := s.CreateBackupJob(ctx, job); err != nil {
			t.Fatal(err)
		}

		run := &BackupRun{JobID: job.ID, Trigger: "manual"}
		if err := s.StartBackupRun(ctx, run); err != nil {
			t.Fatal(err)
		}

		// 9.5GB: past the int4 ceiling by enough that no amount of treating the column as
		// unsigned would have rescued it. An unremarkable size for a volume snapshot.
		const big int64 = 9_500_000_000
		if err := s.FinishBackupRun(ctx, run.ID, big, "prod/postgres/2026-08-09.sql", nil); err != nil {
			t.Fatalf("recording a %d-byte backup: %v.\n\n"+
				"If this is \"greater than maximum value for int4\", backup_runs.bytes is a "+
				"32-bit INTEGER on Postgres and migration 0017 did not widen it.", big, err)
		}

		runs, err := s.ListBackupRuns(ctx, job.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			t.Fatalf("ListBackupRuns returned %d runs; want 1", len(runs))
		}
		if runs[0].Bytes != big || runs[0].Status != "ok" {
			t.Errorf("the finished run = %+v; want %d bytes and status ok", runs[0], big)
		}
	})
}

// The widening lands on a POPULATED table: any installation that reaches 0017 has run
// backups already, and their byte counts must come through the ALTER unchanged. Postgres
// only — on SQLite the column was always 64-bit and 0017 is a no-op — and it drives the
// schema version directly through the stopAfter seam, which is a package var, so it opens
// its own store rather than going through eachDialect.
func TestMigrate0017WidensBackupBytesOnAPopulatedDatabase(t *testing.T) {
	url := os.Getenv("DAFFA_TEST_PG_URL")
	if url == "" {
		t.Skip("DAFFA_TEST_PG_URL not set — the int4 widening is NOT covered by this run")
	}
	ctx := context.Background()

	stopAfter = "0016_cleanup_policies"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0016: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.db.Exec("DROP SCHEMA IF EXISTS " + quoteIdent(s.pgSchema) + " CASCADE")
		s.Close()
	})

	env := &Environment{Name: "prod"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	target := &StorageTarget{Name: "r2", Endpoint: "https://r2.example.com", Bucket: "backups",
		KeyID: "k", SecretEnc: "sealed"}
	if err := s.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := &BackupJob{EnvID: env.ID, Name: "postgres", Engine: "postgres", Container: "db-1",
		StorageID: target.ID, Encryption: "none"}
	if err := s.CreateBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	// The run an operator already has: just under the old int4 ceiling, which is the largest
	// backup the pre-0017 schema could record at all.
	const old int64 = 2_000_000_000
	run := &BackupRun{JobID: job.ID, Trigger: "schedule"}
	if err := s.StartBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackupRun(ctx, run.ID, old, "prod/postgres/old.sql", nil); err != nil {
		t.Fatalf("recording a pre-0017 run: %v", err)
	}

	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrating to 0017: %v", err)
	}

	var colType string
	if err := s.db.QueryRow(`SELECT data_type FROM information_schema.columns
        WHERE table_schema = $1 AND table_name = 'backup_runs' AND column_name = 'bytes'`,
		s.pgSchema).Scan(&colType); err != nil {
		t.Fatal(err)
	}
	if colType != "bigint" {
		t.Fatalf("backup_runs.bytes is %s after 0017; want bigint", colType)
	}

	// The existing row survived the rewrite with its number.
	runs, err := s.ListBackupRuns(ctx, job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Bytes != old || runs[0].Status != "ok" {
		t.Fatalf("the pre-0017 run did not survive the migration: %+v", runs)
	}

	// The big write goes through a REOPENED store, because pgx caches each statement's
	// parameter OIDs per connection and this one described that UPDATE while the column was
	// still int4 — it would refuse to encode client-side, never reaching the widened column.
	// Production never sees that: migrations run inside Open, before a single handler query.
	s2, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("reopen after 0017: %v", err)
	}
	defer s2.Close()

	// The same row now takes a size the old column could not hold.
	const big int64 = 9_500_000_000
	if err := s2.FinishBackupRun(ctx, run.ID, big, "prod/postgres/new.sql", nil); err != nil {
		t.Fatalf("recording a %d-byte backup after 0017: %v", big, err)
	}
	if runs, err = s2.ListBackupRuns(ctx, job.ID, 10); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Bytes != big {
		t.Errorf("the widened run = %+v; want %d bytes", runs, big)
	}
}
