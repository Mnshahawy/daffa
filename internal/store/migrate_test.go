package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// 0008 gives an agent a join target — which cluster's Swarm it joins on connect — as a real
// foreign key. A pre-existing agent must survive the migration with a NULL (standalone) target, and
// the foreign key must actually bite: removing the cluster an agent targets takes the agent with it.
func TestMigrate0008AgentJoinTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url := "sqlite://" + filepath.Join(dir, "test.db")

	stopAfter = "0007_gitcred_ssh_key_ref"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0007: %v", err)
	}
	defer s.Close()

	// A 0007-era agent — no join columns yet.
	if _, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO agents
        (id, name, token_hash, version, last_seen_at, created_at, created_by)
        VALUES ('ag_legacy', 'old', NULL, '', NULL, ?, NULL)`), ts(now())); err != nil {
		t.Fatalf("insert 0007-era agent: %v", err)
	}

	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrating to 0008: %v", err)
	}

	// The pre-existing agent survives, standalone (no target), with the defaulted role.
	got, err := s.AgentByID(ctx, "ag_legacy")
	if err != nil {
		t.Fatalf("reading migrated agent: %v", err)
	}
	if got.JoinEnvID != "" || got.JoinRole != "worker" {
		t.Errorf("migrated agent has unexpected join fields: %+v", got)
	}

	// A new agent targeting a cluster round-trips, and the FK cascade removes it with the cluster.
	env := &Environment{Name: "prod"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	ag := &Agent{Name: "worker-1", JoinEnvID: env.ID, JoinRole: "worker"}
	if err := s.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AgentByID(ctx, ag.ID); err != nil || got.JoinEnvID != env.ID {
		t.Fatalf("targeted agent JoinEnvID = %q, err %v; want %q", got.JoinEnvID, err, env.ID)
	}

	if err := s.DeleteEnvironment(ctx, env.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AgentByID(ctx, ag.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing the cluster should cascade-delete its agent; got err %v", err)
	}
}

// 0009 rebuilds the certificates table (per-env names) — and on SQLite, with foreign_keys(1),
// a careless rebuild would cascade-DELETE every cert_deliveries row via the implicit DELETE that
// DROP TABLE performs on the referenced parent. This test builds the 0008 world with CAs, certs
// and BOTH kinds of delivery (cert-carrying and trust-bundle-only), migrates forward for real,
// and asserts the deliveries survived intact — that assertion is the whole point. It then proves
// the new uniqueness rule: same name in two envs OK, duplicate within an env (or shared) refused.
func TestMigrate0009CertEnvScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url := "sqlite://" + filepath.Join(dir, "test.db")

	stopAfter = "0008_agent_join_target"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0008: %v", err)
	}
	defer s.Close()

	env := &Environment{Name: "prod"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}

	// 0008-era rows, raw SQL with the column sets of that era (no outbound_trust, no
	// env_id/usages, no bundle_cas).
	for _, ins := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO cert_authorities (id, name, subject, cert_pem, key_enc, key_algo,
            not_before, not_after, status, rotates_id, overlap_until, warn_days, created_at, created_by, protected)
            VALUES ('ca_old', 'internal-ca', 'CN=Old', 'PEM', 'sealed', 'ecdsa-p256', ?, ?, 'active', NULL, NULL, 180, ?, NULL, 0)`,
			[]any{ts(now()), ts(now()), ts(now())}},
		{`INSERT INTO certificates (id, name, ca_id, sans, key_algo, cert_pem, chain_pem, key_enc,
            not_before, not_after, validity_days, renew_before_days, status, last_error, created_at, created_by, protected)
            VALUES ('crt_old', 'cellauth', 'ca_old', 'cellauth', 'ecdsa-p256', 'PEM', '', 'sealed', ?, ?, 398, 30, 'ok', '', ?, NULL, 0)`,
			[]any{ts(now()), ts(now()), ts(now())}},
		{`INSERT INTO cert_deliveries (id, env_id, cert_id, volume, uid, gid, traefik, restart_targets,
            synced_hash, synced_at, status, last_error, created_at, created_by, protected)
            VALUES ('dlv_cert', ?, 'crt_old', 'certs-cellauth', 100, 100, 0, 'cellauth', 'hash1', NULL, 'ok', '', ?, NULL, 0)`,
			[]any{env.ID, ts(now())}},
		{`INSERT INTO cert_deliveries (id, env_id, cert_id, volume, uid, gid, traefik, restart_targets,
            synced_hash, synced_at, status, last_error, created_at, created_by, protected)
            VALUES ('dlv_bundle', ?, NULL, 'trust-only', 0, 0, 0, '', 'hash2', NULL, 'ok', '', ?, NULL, 0)`,
			[]any{env.ID, ts(now())}},
	} {
		if _, err := s.db.ExecContext(ctx, s.rebind(ins.q), ins.args...); err != nil {
			t.Fatalf("insert 0008-era row: %v", err)
		}
	}

	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrating to head: %v", err)
	}

	// THE assertion: both deliveries survived the parent rebuild, fields intact — twice
	// over now, since 0013 rebuilds cert_deliveries a second time to drop cert_id.
	for _, want := range []struct{ id, certID, volume, hash string }{
		{"dlv_cert", "crt_old", "certs-cellauth", "hash1"},
		{"dlv_bundle", "", "trust-only", "hash2"},
	} {
		d, err := s.CertDeliveryByID(ctx, want.id)
		if err != nil {
			t.Fatalf("delivery %s did not survive the rebuild: %v", want.id, err)
		}
		if d.Volume != want.volume || d.SyncedHash != want.hash {
			t.Errorf("delivery %s mangled by the rebuild: %+v", want.id, d)
		}
		// 0013 moved cert_id into the join table, defaulting the one certificate a
		// single-cert delivery carried — which is what its rendered fragment already said.
		if want.certID == "" {
			if len(d.Certs) != 0 {
				t.Errorf("trust-bundle-only delivery gained certificates: %+v", d.Certs)
			}
			continue
		}
		if len(d.Certs) != 1 || d.Certs[0].CertID != want.certID || !d.Certs[0].IsDefault {
			t.Errorf("delivery %s: certs = %+v, want just %s as the default", want.id, d.Certs, want.certID)
		}
		if d.MountPath != DefaultCertMountPath {
			t.Errorf("delivery %s: mount path = %q, want the pre-0013 default", want.id, d.MountPath)
		}
	}

	// The pre-existing cert reads back shared, with the defaulted usages.
	old, err := s.CertificateByID(ctx, "crt_old")
	if err != nil {
		t.Fatal(err)
	}
	if old.EnvID != "" || old.Usages != "server" || old.SANs != "cellauth" {
		t.Errorf("migrated cert: %+v", old)
	}
	// And the pre-existing CA keeps outbound trust (the prior behaviour).
	if ca, err := s.CertAuthorityByID(ctx, "ca_old"); err != nil || !ca.OutboundTrust {
		t.Errorf("migrated CA should keep outbound trust: %+v, err %v", ca, err)
	}

	// Per-env uniqueness: the same name lands in staging and prod, but not twice in one
	// env — and not twice shared.
	staging := &Environment{Name: "staging"}
	if err := s.CreateEnvironment(ctx, staging); err != nil {
		t.Fatal(err)
	}
	a := &Certificate{Name: "cellauth", EnvID: env.ID, CertPEM: "PEM", KeyEnc: "sealed"}
	if err := s.CreateCertificate(ctx, a); err != nil {
		t.Fatalf("same name in a different env must be allowed: %v", err)
	}
	b := &Certificate{Name: "cellauth", EnvID: staging.ID, CertPEM: "PEM", KeyEnc: "sealed"}
	if err := s.CreateCertificate(ctx, b); err != nil {
		t.Fatalf("same name in a second env must be allowed: %v", err)
	}
	if err := s.CreateCertificate(ctx, &Certificate{Name: "cellauth", EnvID: env.ID, CertPEM: "PEM", KeyEnc: "s"}); !IsDuplicate(err) {
		t.Errorf("duplicate name within one env: err %v, want a unique violation", err)
	}
	if err := s.CreateCertificate(ctx, &Certificate{Name: "cellauth", CertPEM: "PEM", KeyEnc: "s"}); !IsDuplicate(err) {
		t.Errorf("duplicate SHARED name (crt_old is shared): err %v, want a unique violation", err)
	}

	// The visibility filter: an env-limited caller sees shared + their env, not the other.
	visible, err := s.ListCertificates(ctx, false, []string{staging.ID})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range visible {
		ids[c.ID] = true
	}
	if !ids["crt_old"] || !ids[b.ID] || ids[a.ID] {
		t.Errorf("env-limited list saw %v; want shared + staging only", ids)
	}

	// Env deletion cascades its certs.
	if err := s.DeleteEnvironment(ctx, staging.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CertificateByID(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting an env should cascade its certs; got err %v", err)
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

// 0013 has to survive data the new unique index forbids. Two Traefik deliveries on one
// volume are possible in every pre-0013 database — both writing tls.yml, both reporting ok,
// one silently overwriting the other — and creating the index over them would abort the
// migration. A Daffa that will not start is a Daffa the operator cannot use to fix the
// problem, so the migration demotes the losers and says so instead.
func TestMigrate0013DemotesDuplicateTraefikDeliveries(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(t.TempDir(), "test.db")

	stopAfter = "0012_ca_outbound_trust"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0012: %v", err)
	}
	defer s.Close()

	env := &Environment{Name: "prod"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	// Two Traefik deliveries fighting over one volume, plus one innocent bystander on
	// another volume that must come through untouched.
	for _, ins := range []struct {
		id, volume string
		traefik    int
		created    string
	}{
		{"dlv_old", "traefik-dynamic", 1, "2026-01-01T00:00:00Z"},
		{"dlv_new", "traefik-dynamic", 1, "2026-02-01T00:00:00Z"},
		{"dlv_other", "other-dynamic", 1, "2026-03-01T00:00:00Z"},
	} {
		if _, err := s.db.ExecContext(ctx, s.rebind(
			`INSERT INTO cert_deliveries (id, env_id, cert_id, volume, uid, gid, traefik,
                restart_targets, synced_hash, synced_at, status, last_error, created_at,
                created_by, protected, bundle_cas)
             VALUES (?, ?, NULL, ?, 0, 0, ?, '', 'h', NULL, 'ok', '', ?, NULL, 0, '')`),
			ins.id, env.ID, ins.volume, ins.traefik, ins.created); err != nil {
			t.Fatalf("insert 0012-era delivery: %v", err)
		}
	}

	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("0013 aborted on data the index forbids — it must demote, not fail: %v", err)
	}

	newest, err := s.CertDeliveryByID(ctx, "dlv_new")
	if err != nil {
		t.Fatal(err)
	}
	if !newest.Traefik || newest.Status != "ok" {
		t.Errorf("the newest delivery should keep the fragment: %+v", newest)
	}
	loser, err := s.CertDeliveryByID(ctx, "dlv_old")
	if err != nil {
		t.Fatal(err)
	}
	if loser.Traefik {
		t.Error("the older duplicate kept rendering tls.yml; the two would still overwrite each other")
	}
	if loser.Status != "error" || !strings.Contains(loser.LastError, "dlv_new") {
		t.Errorf("a demoted delivery must say so, naming the winner: status %q, error %q",
			loser.Status, loser.LastError)
	}
	if other, err := s.CertDeliveryByID(ctx, "dlv_other"); err != nil || !other.Traefik {
		t.Errorf("a delivery on its own volume was demoted: %+v, err %v", other, err)
	}
}

// 0013 is one of the few migrations whose dialects genuinely diverge — SQLite rebuilds the
// table to drop cert_id, Postgres just drops the column — and the box it runs on in
// production is a Postgres box with real deliveries in it. A fresh-schema Postgres run
// proves nothing about that: it never executes the backfill against rows that exist. So
// this builds the 0012 world in Postgres, populates it, and migrates forward for real.
func TestMigrate0013OnPopulatedPostgres(t *testing.T) {
	url := os.Getenv("DAFFA_TEST_PG_URL")
	if url == "" {
		t.Skip("DAFFA_TEST_PG_URL not set — 0013's Postgres branch is NOT covered by this run")
	}
	ctx := context.Background()

	stopAfter = "0012_ca_outbound_trust"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0012: %v", err)
	}
	defer s.Close()
	t.Cleanup(func() {
		_, _ = s.db.Exec("DROP SCHEMA IF EXISTS " + quoteIdent(s.pgSchema) + " CASCADE")
	})

	env := &Environment{Name: "prod"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO certificates (id, name, ca_id, sans, key_algo, cert_pem, chain_pem, key_enc,
            not_before, not_after, validity_days, renew_before_days, status, last_error,
            created_at, created_by, protected, env_id, usages)
         VALUES ('crt_edge', 'daffa-edge', NULL, 'daffa.example', 'ecdsa-p256', 'PEM', '', 'sealed',
            ?, ?, 398, 30, 'ok', '', ?, NULL, 1, NULL, 'server')`),
		ts(now()), ts(now()), ts(now())); err != nil {
		t.Fatalf("insert 0012-era certificate: %v", err)
	}
	// The shape the real box carries: a protected, Traefik-rendering edge delivery.
	if _, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO cert_deliveries (id, env_id, cert_id, volume, uid, gid, traefik,
            restart_targets, synced_hash, synced_at, status, last_error, created_at,
            created_by, protected, bundle_cas)
         VALUES ('dlv_edge', ?, 'crt_edge', 'daffa-edge-certs', 0, 0, 1, '', 'h', NULL, 'ok', '', ?, NULL, 1, '')`),
		env.ID, ts(now())); err != nil {
		t.Fatalf("insert 0012-era delivery: %v", err)
	}

	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("0013 failed on a populated Postgres database: %v", err)
	}

	d, err := s.CertDeliveryByID(ctx, "dlv_edge")
	if err != nil {
		t.Fatalf("the edge delivery did not survive 0013: %v", err)
	}
	if len(d.Certs) != 1 || d.Certs[0].CertID != "crt_edge" || !d.Certs[0].IsDefault {
		t.Errorf("certs = %+v; want crt_edge backfilled as the default", d.Certs)
	}
	if d.MountPath != DefaultCertMountPath || !d.Traefik || !d.Protected || d.Volume != "daffa-edge-certs" {
		t.Errorf("delivery mangled by 0013: %+v", d)
	}
	// cert_id is really gone, not merely unread — a leftover column would be a second
	// source of truth the next writer could disagree with.
	var n int
	if err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT COUNT(*) FROM information_schema.columns
         WHERE table_schema = ? AND table_name = 'cert_deliveries' AND column_name = 'cert_id'`),
		s.pgSchema).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("cert_deliveries.cert_id still exists on Postgres after 0013")
	}
	// And the unique index bites on the real dialect too.
	if _, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO cert_deliveries (id, env_id, volume, uid, gid, traefik, restart_targets,
            synced_hash, synced_at, status, last_error, created_at, created_by, protected,
            bundle_cas, mount_path)
         VALUES ('dlv_second', ?, 'daffa-edge-certs', 0, 0, 1, '', '', NULL, 'pending', '', ?, NULL, 0, '', ?)`),
		env.ID, ts(now()), DefaultCertMountPath); err == nil {
		t.Error("Postgres accepted a second Traefik delivery on the same volume")
	}
}
