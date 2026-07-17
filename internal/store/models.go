package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/caps"
)

// now is a variable so tests can freeze it.
var now = time.Now

// NewID returns a short, URL-safe, sortable-enough identifier. Nothing here needs
// UUID semantics; it needs to be unique and to not look like a database row count.
func NewID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("store: entropy unavailable: %v", err))
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// ── users ───────────────────────────────────────────────────────────────────────

type User struct {
	ID           string
	Kind         string // local | oidc
	Username     string
	PasswordHash string
	Sub          string // oidc: the subject, unique only WITHIN its provider
	OIDCProvider string // oidc: which provider issued Sub
	Email        string
	Disabled     bool
	CreatedAt    time.Time
	LastLoginAt  time.Time

	// Caps is what the user may do, and where: the union of every role they hold, keyed by
	// the scope it was granted at. It is not a column — it is computed (and cached) from
	// role_members, so a permission change takes effect on the next request rather than
	// the next login.
	Caps caps.ScopedMask
}

// Label is what the audit log and UI show for this user.
func (u *User) Label() string {
	switch {
	case u.Username != "":
		return u.Username
	case u.Email != "":
		return u.Email
	default:
		return u.ID
	}
}

const userCols = `id, kind, username, password_hash, sub, oidc_provider_id, email, disabled, created_at, last_login_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var username, hash, sub, provider, lastLogin sql.NullString
	var createdAt string
	var disabled int
	if err := sc.Scan(&u.ID, &u.Kind, &username, &hash, &sub, &provider, &u.Email, &disabled, &createdAt, &lastLogin); err != nil {
		return nil, err
	}
	u.Username, u.PasswordHash, u.Sub, u.OIDCProvider = username.String, hash.String, sub.String, provider.String
	u.Disabled = disabled != 0
	u.CreatedAt = parseTS(createdAt)
	if lastLogin.Valid {
		u.LastLoginAt = parseTS(lastLogin.String)
	}
	return &u, nil
}

func (s *Store) CreateUser(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now()
	}
	_, err := s.exec(ctx, `INSERT INTO users (`+userCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		u.ID, u.Kind, nullStr(u.Username), nullStr(u.PasswordHash), nullStr(u.Sub),
		nullStr(u.OIDCProvider), u.Email, boolInt(u.Disabled), ts(u.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: creating user: %w", err)
	}
	return nil
}

func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	u, err := scanUser(s.queryRow(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// UserBySub looks a user up by the subject their provider issued. The provider is part of
// the key: a `sub` is only unique within an issuer, so two providers can legitimately
// hand out the same one, and matching on `sub` alone would log the second user in as the
// first.
func (s *Store) UserBySub(ctx context.Context, providerID, sub string) (*User, error) {
	u, err := scanUser(s.queryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE oidc_provider_id = ? AND sub = ?`, providerID, sub))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	u, err := scanUser(s.queryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: listing users: %w", err)
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) TouchLogin(ctx context.Context, userID string) error {
	_, err := s.exec(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, ts(now()), userID)
	return err
}

func (s *Store) SetUserEmail(ctx context.Context, userID, email string) error {
	_, err := s.exec(ctx, `UPDATE users SET email = ? WHERE id = ?`, email, userID)
	return err
}

func (s *Store) SetUserPassword(ctx context.Context, userID, hash string) error {
	_, err := s.exec(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

func (s *Store) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	_, err := s.exec(ctx, `UPDATE users SET disabled = ? WHERE id = ?`, boolInt(disabled), userID)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	_, err := s.exec(ctx, `DELETE FROM users WHERE id = ?`, userID)
	return err
}

// ── sessions ────────────────────────────────────────────────────────────────────

type Session struct {
	ID         string // hash of the cookie token
	UserID     string
	BreakGlass bool
	ExpiresAt  time.Time
}

func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	_, err := s.exec(ctx, `INSERT INTO sessions (id, user_id, break_glass, created_at, expires_at)
        VALUES (?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID, boolInt(sess.BreakGlass), ts(now()), ts(sess.ExpiresAt))
	if err != nil {
		return fmt.Errorf("store: creating session: %w", err)
	}
	return nil
}

// SessionUser resolves a session id to its (enabled) user, or ErrNotFound if the
// session is unknown, expired, or its user is disabled.
func (s *Store) SessionUser(ctx context.Context, sessionID string) (*User, *Session, error) {
	row := s.queryRow(ctx, `SELECT s.user_id, s.break_glass, s.expires_at, `+
		`u.id, u.kind, u.username, u.password_hash, u.sub, u.oidc_provider_id, u.email, u.disabled, u.created_at, u.last_login_at `+
		`FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, sessionID)

	var sess Session
	var breakGlass int
	var expires string
	var u User
	var username, hash, sub, provider, lastLogin sql.NullString
	var createdAt string
	var disabled int
	err := row.Scan(&sess.UserID, &breakGlass, &expires,
		&u.ID, &u.Kind, &username, &hash, &sub, &provider, &u.Email, &disabled, &createdAt, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	sess.ID = sessionID
	sess.BreakGlass = breakGlass != 0
	sess.ExpiresAt = parseTS(expires)
	if sess.ExpiresAt.Before(now()) {
		_ = s.DeleteSession(ctx, sessionID)
		return nil, nil, ErrNotFound
	}

	u.Username, u.PasswordHash, u.Sub, u.OIDCProvider = username.String, hash.String, sub.String, provider.String
	u.Disabled = disabled != 0
	u.CreatedAt = parseTS(createdAt)
	if lastLogin.Valid {
		u.LastLoginAt = parseTS(lastLogin.String)
	}
	if u.Disabled {
		return nil, nil, ErrNotFound
	}
	// Caps are deliberately NOT filled here. SessionUser is on the hot path of every
	// request and the mask is cached elsewhere; auth.Manager.Resolve attaches it.
	return &u, &sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE expires_at < ?`, ts(now()))
	return err
}

// ── break-glass ─────────────────────────────────────────────────────────────────

// CreateBreakGlassToken stores the hash of a one-time admin token.
func (s *Store) CreateBreakGlassToken(ctx context.Context, tokenHash string, expiresAt time.Time) error {
	_, err := s.exec(ctx, `INSERT INTO break_glass_tokens (id, created_at, expires_at) VALUES (?, ?, ?)`,
		tokenHash, ts(now()), ts(expiresAt))
	if err != nil {
		return fmt.Errorf("store: creating break-glass token: %w", err)
	}
	return nil
}

// RedeemBreakGlassToken consumes a token, reporting whether it was valid. The row is
// deleted whether or not it had expired, so a token is usable at most once either way.
func (s *Store) RedeemBreakGlassToken(ctx context.Context, tokenHash string) (bool, error) {
	defer s.lockWrites()()

	var expires string
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT expires_at FROM break_glass_tokens WHERE id = ?`), tokenHash).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: reading break-glass token: %w", err)
	}

	if _, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM break_glass_tokens WHERE id = ?`), tokenHash); err != nil {
		return false, fmt.Errorf("store: consuming break-glass token: %w", err)
	}
	return parseTS(expires).After(now()), nil
}

func (s *Store) DeleteExpiredBreakGlassTokens(ctx context.Context) error {
	_, err := s.exec(ctx, `DELETE FROM break_glass_tokens WHERE expires_at < ?`, ts(now()))
	return err
}

// ── environments ────────────────────────────────────────────────────────────────

// Environment is WHERE THINGS RUN: what you select, scope a grant to, and deploy to. It is
// standalone (one node, no Swarm) or a swarm (a cluster of nodes). What it is NOT, any more, is a
// Docker daemon — that is a Node, and an environment is made of them.
type Environment struct {
	ID        string
	Name      string
	SwarmID   string // Info().Swarm.Cluster.ID as the daemon reports it; "" ⇒ standalone
	Status    string
	CreatedAt time.Time
}

// IsSwarm is the whole of the standalone/swarm distinction, and it is derived rather than stored
// for a reason: a `kind` column would be a second copy of a fact this row already carries, and a
// copy that can drift from its original is a bug waiting to be written.
func (e *Environment) IsSwarm() bool { return e.SwarmID != "" }

// UpsertLocalEnvironment finds (or creates) the environment for the local Docker socket, and the
// single node inside it that IS that socket.
//
// It keys on the node's KIND, not on the environment's name: there is exactly one local socket,
// and an admin is free to rename its environment. Looking it up by name would mean a renamed
// environment is not found on the next start, and a duplicate would be created beside it — with
// the name they had just discarded.
func (s *Store) UpsertLocalEnvironment(ctx context.Context, name, dockerHost string) (*Environment, *Node, error) {
	node, err := s.localNode(ctx)
	switch {
	case err == nil:
		env, err := s.EnvironmentByID(ctx, node.EnvID)
		if err != nil {
			return nil, nil, err
		}
		if node.DockerHost != dockerHost {
			if _, err := s.exec(ctx, `UPDATE nodes SET docker_host = ? WHERE id = ?`, dockerHost, node.ID); err != nil {
				return nil, nil, err
			}
			node.DockerHost = dockerHost
		}
		return env, node, nil

	case errors.Is(err, ErrNotFound):
		env := &Environment{ID: NewID(), Name: name, Status: "unknown", CreatedAt: now()}
		if err := s.CreateEnvironment(ctx, env); err != nil {
			return nil, nil, err
		}
		node = &Node{ID: NewID(), EnvID: env.ID, Name: name, Kind: "local", DockerHost: dockerHost, Status: "unknown"}
		if err := s.CreateNode(ctx, node); err != nil {
			return nil, nil, err
		}
		return env, node, nil

	default:
		return nil, nil, err
	}
}

const envCols = `id, name, swarm_id, status, created_at`

func scanEnv(sc interface{ Scan(...any) error }) (*Environment, error) {
	var e Environment
	var createdAt string
	if err := sc.Scan(&e.ID, &e.Name, &e.SwarmID, &e.Status, &createdAt); err != nil {
		return nil, err
	}
	e.CreatedAt = parseTS(createdAt)
	return &e, nil
}

// CreateEnvironment adds an environment row directly. The nodes inside it are created separately,
// because a node is enrolled (or discovered) rather than declared alongside its environment.
func (s *Store) CreateEnvironment(ctx context.Context, e *Environment) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now()
	}
	if e.Status == "" {
		e.Status = "unknown"
	}
	_, err := s.exec(ctx, `INSERT INTO environments (id, name, swarm_id, status, created_at)
        VALUES (?, ?, ?, ?, ?)`,
		e.ID, e.Name, e.SwarmID, e.Status, ts(e.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: creating environment: %w", err)
	}
	return nil
}

func (s *Store) EnvironmentByName(ctx context.Context, name string) (*Environment, error) {
	e, err := scanEnv(s.queryRow(ctx, `SELECT `+envCols+` FROM environments WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

func (s *Store) EnvironmentByID(ctx context.Context, id string) (*Environment, error) {
	e, err := scanEnv(s.queryRow(ctx, `SELECT `+envCols+` FROM environments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// RenameEnvironment gives a host a name a person chose. Names are unique, so the
// switcher never shows two entries you cannot tell apart.
func (s *Store) RenameEnvironment(ctx context.Context, id, name string) error {
	_, err := s.exec(ctx, `UPDATE environments SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("store: renaming environment: %w", err)
	}
	return nil
}

func (s *Store) ListEnvironments(ctx context.Context) ([]*Environment, error) {
	rows, err := s.query(ctx, `SELECT `+envCols+` FROM environments ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: listing environments: %w", err)
	}
	defer rows.Close()
	var out []*Environment
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── audit ───────────────────────────────────────────────────────────────────────

type AuditEntry struct {
	ID        string
	At        time.Time
	UserID    string
	UserLabel string
	EnvID     string
	Action    string
	Target    string
	Outcome   string // ok | error | denied
	Detail    string
}

// Audit writes one entry.
//
// user_label is DENORMALISED on purpose: an audit log whose names disappear when the account
// does is not an audit log, so the label is copied in at write time and never joined at read
// time.
//
// The cost of that is a field every caller has to remember to fill, and most of them did not:
// seven call sites — every deploy outcome, every backup run, stack.cancel, stack.delete,
// stack.autodeploy — set UserID and left UserLabel empty, so the audit page showed a dash in the
// Who column for actions a named person had plainly just taken. Filling it here means no future
// call site can forget, and the ones that already did are fixed by the same three lines.
//
// A genuinely absent user still records nothing, and should: a webhook deploy has no person
// behind it, and a refused login has no authenticated user by definition — the username that was
// TRIED is the target, not the actor. A dash on those rows is the truth.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.At.IsZero() {
		e.At = now()
	}

	if e.UserLabel == "" && e.UserID != "" {
		// Best effort: a lookup that fails must not lose the entry. An audit row with no name is
		// worse than one with a name, and both are far better than no row at all — which is what
		// returning an error here would mean for the paths that ignore it.
		if u, err := s.UserByID(ctx, e.UserID); err == nil && u != nil {
			e.UserLabel = u.Label()
		}
	}

	_, err := s.exec(ctx, `INSERT INTO audit_log (id, at, user_id, user_label, env_id, action, target, outcome, detail)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, ts(e.At), nullStr(e.UserID), e.UserLabel, nullStr(e.EnvID), e.Action, e.Target, e.Outcome, e.Detail)
	if err != nil {
		return fmt.Errorf("store: writing audit entry: %w", err)
	}
	return nil
}

// ListAudit returns the most recent audit entries the caller may see.
//
// envs is the set of hosts they hold audit.view on; global says they hold it fleet-wide.
//
// The filter is in the SQL, and it has to be. LIMIT is applied by the database BEFORE any
// Go-side filter could run, so filtering afterwards would fetch 200 rows, discard most of
// them, and hand back a page of four — no error, no warning, just a log that looks empty.
//
// Rows with no environment — a user created, a role edited, an identity provider changed —
// are fleet-level events and are shown only to a GLOBAL audit.view holder. A staging-scoped
// operator has no business reading who was granted what.
func (s *Store) ListAudit(ctx context.Context, limit int, global bool, envs []string) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	q := `SELECT id, at, user_id, user_label, env_id, action, target, outcome, detail FROM audit_log`
	args := []any{}

	if !global {
		if len(envs) == 0 {
			// Holds audit.view nowhere useful. Return nothing rather than everything —
			// the failure mode of an empty WHERE IN () is the whole table.
			return nil, nil
		}
		ph := make([]string, len(envs))
		for i, e := range envs {
			ph[i] = "?"
			args = append(args, e)
		}
		q += ` WHERE env_id IN (` + strings.Join(ph, ",") + `)`
	}

	q += ` ORDER BY at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing audit: %w", err)
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var userID, envID sql.NullString
		var at string
		if err := rows.Scan(&e.ID, &at, &userID, &e.UserLabel, &envID, &e.Action, &e.Target, &e.Outcome, &e.Detail); err != nil {
			return nil, err
		}
		e.At, e.UserID, e.EnvID = parseTS(at), userID.String, envID.String
		out = append(out, &e)
	}
	return out, rows.Err()
}

// AuditDetail renders a detail payload; it never returns an error, because failing
// to marshal a log line must not fail the operation being logged.
func AuditDetail(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
