package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Mnshahawy/daffa/internal/caps"
)

// A backup_jobs row that predates 0004 must survive the migration with exclude_paths defaulted
// to ” — the whole point of the stopAfter seam is to prove this against a POPULATED older
// database, not a fresh schema. (Every migration bug that ever shipped passed the fresh-schema
// tests.) SQLite-only: the seam is a package var, so this test controls the schema version
// directly and cannot run under a shared connection.
func TestMigrate0004PreservesOlderBackupJobs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url := "sqlite://" + filepath.Join(dir, "test.db")

	// Bring the schema up to 0003 and no further, then open a store on it.
	stopAfter = "0003_inline_volume_sources"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0003: %v", err)
	}
	defer s.Close()

	// The parents the FK constraints require, written through the store (their tables are
	// unchanged since 0001, so today's Create* code matches the 0003 schema).
	env := &Environment{Name: "prod"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	target := &StorageTarget{Name: "r2", Endpoint: "https://r2.example.com", Bucket: "backups",
		KeyID: "k", SecretEnc: "sealed"}
	if err := s.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}

	// A raw insert with the 0003-era column set — no exclude_paths, because at 0003 that column
	// does not exist yet. This is the row an operator would already have.
	if _, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO backup_jobs
        (id, env_id, name, container, engine, databases, db_user, db_password_enc, schedule,
         storage_id, prefix, encryption, volume, stop_containers, enabled, created_at, created_by)
        VALUES (?, ?, ?, '', 'volume', '', '', '', '', ?, '', 'none', 'legacy-data', '', 1, ?, NULL)`),
		"job_legacy", env.ID, "legacy", target.ID, ts(now())); err != nil {
		t.Fatalf("insert 0003-era row: %v", err)
	}

	// Now finish the migrations for real — 0004 adds exclude_paths to the populated table.
	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrating to 0004: %v", err)
	}

	// The pre-existing row reads back through the store (scanJob now expects the new column) and
	// carries the '' default.
	got, err := s.BackupJobByID(ctx, "job_legacy")
	if err != nil {
		t.Fatalf("reading migrated row: %v", err)
	}
	if got.ExcludePaths != "" {
		t.Errorf("migrated row ExcludePaths = %q; want empty", got.ExcludePaths)
	}
	if got.Volume != "legacy-data" {
		t.Errorf("migrated row lost its volume: got %q", got.Volume)
	}

	// And a job created after the migration round-trips a real value.
	fresh := &BackupJob{EnvID: env.ID, Name: "fresh", Engine: "volume", Volume: "d",
		StorageID: target.ID, Encryption: "none", ExcludePaths: "cache\nlogs"}
	if err := s.CreateBackupJob(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if got, err := s.BackupJobByID(ctx, fresh.ID); err != nil || got.ExcludePaths != "cache\nlogs" {
		t.Fatalf("fresh job ExcludePaths = %q, err %v; want %q", got.ExcludePaths, err, "cache\nlogs")
	}
}

// 0007 adds the key-store reference and DROPS the old inline key columns unconditionally. A
// credential that predates it must survive the migration — its identity intact, its key reference
// empty (the material cannot be auto-migrated, so it is re-selected under Settings → SSH keys).
// This proves the DROP COLUMN runs against a POPULATED table and does not take the row with it.
func TestMigrate0007DropsGitCredKeyColumns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url := "sqlite://" + filepath.Join(dir, "test.db")

	stopAfter = "0006_ssh_nodes"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0006: %v", err)
	}
	defer s.Close()

	// A 0006-era SSH credential: it carries its own sealed key in the columns 0007 removes.
	if _, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO git_credentials
        (id, name, kind, username, token_enc, ssh_key_enc, passphrase_enc, host_key, created_at, created_by)
        VALUES ('gc_legacy', 'legacy-deploy', 'ssh', '', '', 'sealed-key', 'sealed-pass', '', ?, NULL)`),
		ts(now())); err != nil {
		t.Fatalf("insert 0006-era row: %v", err)
	}

	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrating to 0007: %v", err)
	}

	// The row reads back through the store (scanGitCred no longer scans the dropped columns), with
	// its identity intact and no key reference — the state the UI surfaces as "re-select a key".
	got, err := s.GitCredentialByID(ctx, "gc_legacy")
	if err != nil {
		t.Fatalf("reading migrated row: %v", err)
	}
	if got.Name != "legacy-deploy" || got.Kind != "ssh" {
		t.Errorf("migration lost the credential's identity: %+v", got)
	}
	if got.SSHKeyID != "" {
		t.Errorf("migrated row SSHKeyID = %q; want empty", got.SSHKeyID)
	}

	// A credential created after the migration round-trips its key reference.
	fresh := &GitCredential{Name: "fresh", Kind: GitSSH, SSHKeyID: "sshkey_x"}
	if err := s.CreateGitCredential(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GitCredentialByID(ctx, fresh.ID); err != nil || got.SSHKeyID != "sshkey_x" {
		t.Fatalf("fresh cred SSHKeyID = %q, err %v; want sshkey_x", got.SSHKeyID, err)
	}
}

// A capability mask must be able to hold a HIGH BIT on Postgres, and this test is Postgres-only
// because that is the entire point.
//
// SQLite's INTEGER is 64-bit, so the SQLite path cannot notice a column that is too narrow and
// never will. Postgres's INTEGER is 32-bit and SIGNED, so the highest bit role_caps.mask can
// carry is caps.MaxBit (30). Masks are deliberately small — one INTEGER per namespace, cached in
// memory for every user — and the ceiling is now close enough that only this test would catch a
// column quietly widened past what the code assumes, or narrowed below what a full namespace
// needs, until an area filled up and one particular grant started failing on one dialect.
//
// It writes the bit through raw SQL rather than the store, because the store's Normalize would
// (correctly) discard a bit that no capability owns. The question here is what the COLUMN can
// hold, not what the registry knows.
func TestAMaskColumnHoldsAHighBitOnPostgres(t *testing.T) {
	url := os.Getenv("DAFFA_TEST_PG_URL")
	if url == "" {
		t.Skip("DAFFA_TEST_PG_URL not set — the mask column's width is NOT covered by this run")
	}
	ctx := context.Background()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Cleanup(func() {
		_, _ = s.db.Exec("DROP SCHEMA IF EXISTS " + quoteIdent(s.pgSchema) + " CASCADE")
	})

	// Bit 30 — caps.MaxBit, the highest an area may ever use and still round-trip through a
	// signed 32-bit INTEGER.
	const high = int64(1) << caps.MaxBit

	r := &Role{Name: "Wide", Description: "holds a high bit"}
	if err := s.CreateRole(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO role_caps (role_id, ns, mask) VALUES (?, 'docker', ?)`), r.ID, high); err != nil {
		t.Fatalf("role_caps.mask cannot hold bit %d: %v\n\n"+
			"The column is too narrow for a full namespace — an administrator granting a "+
			"capability in that position would be told \"integer out of range\", and only on "+
			"Postgres.", caps.MaxBit, err)
	}

	var got int64
	if err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT mask FROM role_caps WHERE role_id = ? AND ns = 'docker'`), r.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != high {
		t.Errorf("bit %d did not survive the round trip: stored %d, read back %d", caps.MaxBit, high, got)
	}
}
