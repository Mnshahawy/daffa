package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Every store test runs against both dialects. SQLite always; Postgres only when
// DAFFA_TEST_PG_URL points at a database the test may create a schema in (CI sets
// it via a service container). Skipping quietly would let portability rot, so the
// skip is loud.
func eachDialect(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()

	t.Run("sqlite", func(t *testing.T) {
		s, err := Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		fn(t, s)
	})

	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("DAFFA_TEST_PG_URL")
		if url == "" {
			t.Skip("DAFFA_TEST_PG_URL not set — Postgres dialect NOT covered by this run")
		}
		s, err := Open(context.Background(), url)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		t.Cleanup(func() {
			_, _ = s.db.Exec("DROP SCHEMA IF EXISTS " + quoteIdent(s.pgSchema) + " CASCADE")
			s.Close()
		})
		fn(t, s)
	})
}

func TestUsersAndSessions(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if n, err := s.CountUsers(ctx); err != nil || n != 0 {
			t.Fatalf("CountUsers on fresh store = %d, %v; want 0, nil", n, err)
		}

		u := &User{Kind: "local", Username: "admin", PasswordHash: "hash"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		got, err := s.UserByUsername(ctx, "admin")
		if err != nil {
			t.Fatalf("UserByUsername: %v", err)
		}
		if got.ID != u.ID || got.PasswordHash != "hash" {
			t.Fatalf("UserByUsername = %+v; want id=%s", got, u.ID)
		}
		if got.Disabled {
			t.Fatal("new user is disabled; want enabled")
		}

		if _, err := s.UserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("UserByUsername(nobody) error = %v; want ErrNotFound", err)
		}

		// A session resolves to its user...
		sess := &Session{ID: "sess-hash", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		su, ss, err := s.SessionUser(ctx, "sess-hash")
		if err != nil {
			t.Fatalf("SessionUser: %v", err)
		}
		if su.ID != u.ID || ss.BreakGlass {
			t.Fatalf("SessionUser = %+v, %+v; want user %s, break_glass=false", su, ss, u.ID)
		}

		// ...but not once the user is disabled.
		if err := s.SetUserDisabled(ctx, u.ID, true); err != nil {
			t.Fatalf("SetUserDisabled: %v", err)
		}
		if _, _, err := s.SessionUser(ctx, "sess-hash"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SessionUser for disabled user error = %v; want ErrNotFound", err)
		}
		if err := s.SetUserDisabled(ctx, u.ID, false); err != nil {
			t.Fatalf("re-enable: %v", err)
		}

		// ...and not once it has expired (and the expired row is reaped).
		expired := &Session{ID: "old", UserID: u.ID, ExpiresAt: time.Now().Add(-time.Minute)}
		if err := s.CreateSession(ctx, expired); err != nil {
			t.Fatalf("CreateSession(expired): %v", err)
		}
		if _, _, err := s.SessionUser(ctx, "old"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SessionUser(expired) error = %v; want ErrNotFound", err)
		}

		// Deleting the user cascades to sessions.
		if err := s.DeleteUser(ctx, u.ID); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		if _, _, err := s.SessionUser(ctx, "sess-hash"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session survived user deletion: %v", err)
		}
	})
}

func TestEnvironmentsUpsert(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		env, node, err := s.UpsertLocalEnvironment(ctx, "local", "unix:///var/run/docker.sock")
		if err != nil {
			t.Fatalf("UpsertLocalEnvironment: %v", err)
		}
		if node.EnvID != env.ID {
			t.Fatalf("the local node landed in env %s; want %s", node.EnvID, env.ID)
		}
		if env.IsSwarm() {
			t.Fatal("a freshly created local environment reports itself as a swarm")
		}

		// Upsert is idempotent, and a changed socket is updated in place — on the NODE, which is
		// the thing that has a socket. The environment is where things run; the node is the daemon.
		againEnv, againNode, err := s.UpsertLocalEnvironment(ctx, "local", "unix:///run/docker.sock")
		if err != nil {
			t.Fatalf("UpsertLocalEnvironment (again): %v", err)
		}
		if againEnv.ID != env.ID {
			t.Fatalf("upsert minted a new environment id %s; want stable %s", againEnv.ID, env.ID)
		}
		if againNode.ID != node.ID {
			t.Fatalf("upsert minted a new node id %s; want stable %s", againNode.ID, node.ID)
		}
		if againNode.DockerHost != "unix:///run/docker.sock" {
			t.Fatalf("node DockerHost = %q; want the updated socket", againNode.DockerHost)
		}

		envs, err := s.ListEnvironments(ctx)
		if err != nil {
			t.Fatalf("ListEnvironments: %v", err)
		}
		if len(envs) != 1 {
			t.Fatalf("ListEnvironments returned %d envs; want 1", len(envs))
		}

		// One environment, one node: that is what a standalone environment IS, and the upsert must
		// not have quietly added a second daemon to it.
		nodes, err := s.NodesByEnv(ctx, env.ID)
		if err != nil {
			t.Fatalf("NodesByEnv: %v", err)
		}
		if len(nodes) != 1 {
			t.Fatalf("the local environment has %d nodes; want exactly 1", len(nodes))
		}
	})
}

func TestAuditLog(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		// An audit entry must survive the user it refers to — that's the point of
		// denormalizing the label.
		u := &User{Kind: "local", Username: "op"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		err := s.Audit(ctx, AuditEntry{
			UserID: u.ID, UserLabel: u.Label(), Action: "container.restart",
			Target: "abc123", Outcome: "ok", Detail: AuditDetail(map[string]string{"env": "local"}),
		})
		if err != nil {
			t.Fatalf("Audit: %v", err)
		}
		if err := s.DeleteUser(ctx, u.ID); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}

		entries, err := s.ListAudit(ctx, 10, true, nil)
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("ListAudit returned %d entries; want 1 (audit must outlive the user)", len(entries))
		}
		if entries[0].UserLabel != "op" || entries[0].Action != "container.restart" {
			t.Fatalf("audit entry = %+v; want label=op action=container.restart", entries[0])
		}
	})
}

// Audit fills in the label when the caller only knew the id.
//
// The label is denormalized so the log outlives the account, and the price of that is a field
// every caller has to remember. Most of them didn't: every deploy outcome, every backup run,
// stack.cancel, stack.delete and stack.autodeploy all passed a UserID and no UserLabel — so the
// audit page showed "—" under Who for a deploy a named person had just clicked, and the record
// of who did it existed only as an opaque id nothing rendered.
//
// The backfill lives in Audit() rather than at each call site, so a new one cannot reintroduce
// this by omission.
func TestAuditFillsTheLabelFromTheID(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		u := &User{Kind: "local", Username: "deployer"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		// The deploy path's exact shape: an id, and no label.
		if err := s.Audit(ctx, AuditEntry{
			UserID: u.ID, Action: "stack.up", Target: "api", Outcome: "ok",
		}); err != nil {
			t.Fatalf("Audit: %v", err)
		}

		// A webhook deploy has no person behind it, and a refused login has no authenticated user
		// by definition. Those must stay blank rather than acquire a name from nowhere.
		if err := s.Audit(ctx, AuditEntry{
			Action: "stack.webhook", Target: "api", Outcome: "ok",
		}); err != nil {
			t.Fatalf("Audit (no user): %v", err)
		}

		entries, err := s.ListAudit(ctx, 10, true, nil)
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}

		byAction := map[string]*AuditEntry{}
		for _, e := range entries {
			byAction[e.Action] = e
		}

		if got := byAction["stack.up"]; got == nil || got.UserLabel != "deployer" {
			t.Fatalf("stack.up UserLabel = %q; want %q — a deploy someone started must name them",
				labelOf(got), "deployer")
		}
		if got := byAction["stack.webhook"]; got == nil || got.UserLabel != "" {
			t.Fatalf("stack.webhook UserLabel = %q; want empty — nobody started it",
				labelOf(got))
		}
	})
}

func labelOf(e *AuditEntry) string {
	if e == nil {
		return "<missing entry>"
	}
	return e.UserLabel
}

func TestMigrateIsIdempotent(t *testing.T) {
	// Reopening an existing store must not re-run migrations.
	dir := t.TempDir()
	url := "sqlite://" + filepath.Join(dir, "test.db")
	ctx := context.Background()

	s1, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	u := &User{Kind: "local", Username: "keep"}
	if err := s1.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	s1.Close()

	s2, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("second open (migrations should be a no-op): %v", err)
	}
	defer s2.Close()
	if _, err := s2.UserByUsername(ctx, "keep"); err != nil {
		t.Fatalf("data did not survive reopen: %v", err)
	}
}
