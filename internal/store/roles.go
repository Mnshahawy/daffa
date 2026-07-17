package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/caps"
)

// ── roles ───────────────────────────────────────────────────────────────────────

type Role struct {
	ID          string
	Name        string
	Description string
	Caps        caps.Set // one mask per functional area; see role_caps
	IsAdmin     bool     // ⇒ every capability, resolved at runtime
	Builtin     bool     // ⇒ cannot be deleted
	CreatedAt   time.Time
	Members     int // populated by ListRoles only
}

// Effective is what the role actually grants. An admin role resolves to the whole registry
// HERE rather than storing an all-ones set, so a capability added in a later version is held by
// admins the moment it exists — a stored set would leave every admin silently short of it.
func (r *Role) Effective() caps.Set {
	if r.IsAdmin {
		return caps.Everything
	}
	return r.Caps
}

// ErrLastAdmin is returned by any write that would leave the system with no way back in.
var ErrLastAdmin = errors.New("store: this would remove the last administrator")

// ErrBuiltinRole is returned when someone tries to delete an undeletable role.
var ErrBuiltinRole = errors.New("store: this role is built in and cannot be deleted")

const roleCols = `id, name, description, is_admin, builtin, created_at`

func scanRole(sc interface{ Scan(...any) error }) (*Role, error) {
	var r Role
	var isAdmin, builtin int
	var createdAt string
	if err := sc.Scan(&r.ID, &r.Name, &r.Description, &isAdmin, &builtin, &createdAt); err != nil {
		return nil, err
	}
	r.IsAdmin, r.Builtin = isAdmin != 0, builtin != 0
	r.CreatedAt = parseTS(createdAt)
	return &r, nil
}

// capsOf loads the per-area masks for a set of roles, in ONE query.
//
// One query rather than one per role: the role list is small, but a query per row is the habit
// that turns a small list into a slow page the moment somebody makes fifty roles.
//
// The masks are Normalized on the way out as well as in. A row hand-edited in the database, one
// carrying a bit from a capability we have since retired, or one written by a NEWER Daffa in an
// area this build has never heard of, must not leak into a permission check.
func (s *Store) capsOf(ctx context.Context, roleIDs []string) (map[string]caps.Set, error) {
	out := map[string]caps.Set{}
	if len(roleIDs) == 0 {
		return out, nil
	}

	ph := make([]string, len(roleIDs))
	args := make([]any, len(roleIDs))
	for i, id := range roleIDs {
		ph[i], args[i] = "?", id
	}

	rows, err := s.query(ctx,
		`SELECT role_id, ns, mask FROM role_caps WHERE role_id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: loading role capabilities: %w", err)
	}
	defer rows.Close()

	raw := map[string]caps.Set{}
	for rows.Next() {
		var roleID, ns string
		var mask int64
		if err := rows.Scan(&roleID, &ns, &mask); err != nil {
			return nil, err
		}
		if raw[roleID] == nil {
			raw[roleID] = caps.Set{}
		}
		raw[roleID][caps.Namespace(ns)] = caps.Mask(mask)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for id, set := range raw {
		out[id] = caps.Normalize(set)
	}
	return out, nil
}

// withCaps fills in the capability sets of roles already read.
func (s *Store) withCaps(ctx context.Context, roles []*Role) error {
	ids := make([]string, 0, len(roles))
	for _, r := range roles {
		ids = append(ids, r.ID)
	}
	sets, err := s.capsOf(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range roles {
		if set, ok := sets[r.ID]; ok {
			r.Caps = set
		} else {
			r.Caps = caps.Set{}
		}
	}
	return nil
}

func (s *Store) ListRoles(ctx context.Context) ([]*Role, error) {
	rows, err := s.query(ctx, `SELECT r.id, r.name, r.description, r.is_admin, r.builtin, r.created_at,
        (SELECT COUNT(*) FROM role_members rm WHERE rm.role_id = r.id)
        FROM roles r ORDER BY r.is_admin DESC, r.name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing roles: %w", err)
	}
	defer rows.Close()

	var out []*Role
	for rows.Next() {
		var r Role
		var isAdmin, builtin int
		var createdAt string
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &isAdmin, &builtin, &createdAt, &r.Members); err != nil {
			return nil, err
		}
		r.IsAdmin, r.Builtin = isAdmin != 0, builtin != 0
		r.CreatedAt = parseTS(createdAt)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := s.withCaps(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// oneRole reads a single role and its capabilities.
func (s *Store) oneRole(ctx context.Context, where string, args ...any) (*Role, error) {
	r, err := scanRole(s.queryRow(ctx, `SELECT `+roleCols+` FROM roles `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.withCaps(ctx, []*Role{r}); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) RoleByID(ctx context.Context, id string) (*Role, error) {
	return s.oneRole(ctx, `WHERE id = ?`, id)
}

func (s *Store) RoleByName(ctx context.Context, name string) (*Role, error) {
	return s.oneRole(ctx, `WHERE name = ?`, name)
}

// AdminRole is the builtin all-capabilities role. The CLI and break-glass need it by something
// other than a hardcoded id, and it is the one role guaranteed to exist.
func (s *Store) AdminRole(ctx context.Context) (*Role, error) {
	return s.oneRole(ctx, `WHERE is_admin = 1 AND builtin = 1 ORDER BY created_at LIMIT 1`)
}

func (s *Store) CreateRole(ctx context.Context, r *Role) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now()
	}

	defer s.lockWrites()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO roles (`+roleCols+`) VALUES (?, ?, ?, ?, ?, ?)`),
		r.ID, r.Name, r.Description, boolInt(r.IsAdmin), boolInt(r.Builtin), ts(r.CreatedAt)); err != nil {
		return fmt.Errorf("store: creating role: %w", err)
	}
	if err := s.writeCaps(ctx, tx, r.ID, r.Caps); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateRole changes a role's name, description and capabilities.
//
// It cannot change is_admin or builtin. Those are structural: letting an admin role be demoted
// through the ordinary edit path is exactly how a system ends up with no administrator and a
// very confused operator.
func (s *Store) UpdateRole(ctx context.Context, r *Role) error {
	defer s.lockWrites()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.rebind(
		`UPDATE roles SET name = ?, description = ? WHERE id = ?`), r.Name, r.Description, r.ID)
	if err != nil {
		return fmt.Errorf("store: updating role: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	// The capabilities and the role must move together. If the masks were written outside the
	// transaction, a failure between them would leave a role whose NAME says one thing and whose
	// permissions say another — and the half that lands would be the permissions.
	if err := s.writeCaps(ctx, tx, r.ID, r.Caps); err != nil {
		return err
	}
	return tx.Commit()
}

// writeCaps replaces a role's per-area masks. Delete-then-insert, in the caller's transaction.
//
// An area whose mask is zero gets NO ROW rather than a row of zeroes. A missing area and an
// empty one mean the same thing, and storing both spellings is how a "does this role grant
// anything in Backups?" query ends up with two answers.
func (s *Store) writeCaps(ctx context.Context, tx *sql.Tx, roleID string, set caps.Set) error {
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM role_caps WHERE role_id = ?`), roleID); err != nil {
		return fmt.Errorf("store: clearing role capabilities: %w", err)
	}

	for ns, mask := range caps.Normalize(set) {
		if mask == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO role_caps (role_id, ns, mask) VALUES (?, ?, ?)`),
			roleID, string(ns), int64(mask)); err != nil {
			return fmt.Errorf("store: writing role capabilities: %w", err)
		}
	}
	return nil
}

// DeleteRole removes a role, and with it every membership (the FK cascades).
func (s *Store) DeleteRole(ctx context.Context, id string) error {
	r, err := s.RoleByID(ctx, id)
	if err != nil {
		return err
	}
	if r.Builtin {
		return ErrBuiltinRole
	}
	// A non-builtin admin role could still be the only one anybody holds.
	if r.IsAdmin {
		n, err := s.countOtherAdmins(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrLastAdmin
		}
	}
	_, err = s.exec(ctx, `DELETE FROM roles WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting role: %w", err)
	}
	return nil
}

// countOtherAdmins counts enabled users holding an admin role OTHER than excludeRole.
// It is the single question behind every lockout guard: "if I do this, is anybody left?"
func (s *Store) countOtherAdmins(ctx context.Context, excludeRole string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(DISTINCT u.id)
        FROM users u
        JOIN role_members rm ON rm.user_id = u.id
        JOIN roles r ON r.id = rm.role_id
        WHERE r.is_admin = 1 AND u.disabled = 0 AND r.id <> ?`, excludeRole).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting administrators: %w", err)
	}
	return n, nil
}

// countAdminsExcludingUser counts enabled admins other than excludeUser.
func (s *Store) countAdminsExcludingUser(ctx context.Context, excludeUser string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(DISTINCT u.id)
        FROM users u
        JOIN role_members rm ON rm.user_id = u.id
        JOIN roles r ON r.id = rm.role_id
        WHERE r.is_admin = 1 AND u.disabled = 0 AND u.id <> ?`, excludeUser).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting administrators: %w", err)
	}
	return n, nil
}

// IsAdminUser reports whether the user holds any admin role.
func (s *Store) IsAdminUser(ctx context.Context, userID string) (bool, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*)
        FROM role_members rm JOIN roles r ON r.id = rm.role_id
        WHERE rm.user_id = ? AND r.is_admin = 1`, userID).Scan(&n)
	return n > 0, err
}

// ── memberships ─────────────────────────────────────────────────────────────────

// Scope is where a grant applies: everywhere, or on one host.
type Scope struct {
	Kind string // ScopeGlobal | ScopeEnv
	ID   string // env id when Kind == ScopeEnv, "" otherwise
}

const (
	ScopeGlobal = "global"
	ScopeEnv    = "env"
)

// Global is the fleet-wide scope. Named, because `Scope{"global", ""}` at forty call sites
// is forty chances to write `Scope{"env", ""}` by accident — which would be a grant on a
// host whose id is the empty string, i.e. a grant that silently does nothing.
func Global() Scope { return Scope{Kind: ScopeGlobal} }

// OnEnv is a grant on one host.
func OnEnv(envID string) Scope { return Scope{Kind: ScopeEnv, ID: envID} }

func (s Scope) IsGlobal() bool { return s.Kind == ScopeGlobal }

// Valid rejects the shapes that would be silently useless: an env scope with no host, or a
// global scope that carries one.
func (s Scope) Valid() bool {
	switch s.Kind {
	case ScopeGlobal:
		return s.ID == ""
	case ScopeEnv:
		return s.ID != ""
	default:
		return false
	}
}

// ErrCannotScope is returned when a role holding a global-only capability is granted on one
// host. See docs/scoping.md — this is the rule the whole scoped model rests on.
var ErrCannotScope = errors.New("store: this role cannot be limited to one host")

// Membership is one role a user holds, where the grant came from, and where it applies.
type Membership struct {
	RoleID  string
	Name    string
	IsAdmin bool
	Source  string // local | oidc
	Scope   Scope
	EnvName string // resolved, for display; empty for a global grant
}

const (
	SourceLocal = "local"
	SourceOIDC  = "oidc"
)

func (s *Store) UserRoles(ctx context.Context, userID string) ([]Membership, error) {
	// LEFT JOIN: a grant scoped to a host that has since been deleted must still be
	// listed, not silently dropped from the page an admin uses to fix it.
	rows, err := s.query(ctx, `SELECT r.id, r.name, r.is_admin, rm.source, rm.scope_kind,
            rm.scope_id, COALESCE(e.name, '')
        FROM role_members rm
        JOIN roles r ON r.id = rm.role_id
        LEFT JOIN environments e ON e.id = rm.scope_id
        WHERE rm.user_id = ?
        ORDER BY r.is_admin DESC, r.name, rm.scope_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing user roles: %w", err)
	}
	defer rows.Close()

	var out []Membership
	for rows.Next() {
		var m Membership
		var isAdmin int
		if err := rows.Scan(&m.RoleID, &m.Name, &isAdmin, &m.Source,
			&m.Scope.Kind, &m.Scope.ID, &m.EnvName); err != nil {
			return nil, err
		}
		m.IsAdmin = isAdmin != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// EffectiveMask is what the user may do, and where: one set held everywhere, plus a set per
// host.
//
// The join is a LEFT one, and that is load-bearing. An admin role carries NO role_caps rows —
// its power is resolved from the registry at runtime, not stored — so an inner join would drop
// every admin membership on the floor and lock the administrator out of their own console. The
// same is true of any role somebody has emptied.
//
// The OR happens in Go, not in SQL. SQLite has no bit_or aggregate, and the alternative — a
// dialect branch inside the code that decides who may do what — is not a trade worth making to
// avoid a loop over a handful of rows.
//
// This satisfies caps.Loader; the result is cached, so it runs once per user per change.
func (s *Store) EffectiveMask(ctx context.Context, userID string) (caps.ScopedMask, error) {
	rows, err := s.query(ctx, `SELECT rc.ns, rc.mask, r.is_admin, rm.scope_kind, rm.scope_id
        FROM role_members rm
        JOIN roles r ON r.id = rm.role_id
        LEFT JOIN role_caps rc ON rc.role_id = r.id
        WHERE rm.user_id = ?`, userID)
	if err != nil {
		return caps.ScopedMask{}, fmt.Errorf("store: loading capabilities: %w", err)
	}
	defer rows.Close()

	global := caps.Set{}
	env := map[string]caps.Set{}

	for rows.Next() {
		var ns sql.NullString
		var mask sql.NullInt64
		var isAdmin int
		var kind, id string
		if err := rows.Scan(&ns, &mask, &isAdmin, &kind, &id); err != nil {
			return caps.ScopedMask{}, err
		}

		// One row per (membership, area). A membership of an admin role — or of a role that
		// grants nothing — arrives once, with a NULL area.
		row := caps.Set{}
		if ns.Valid && mask.Valid {
			row[caps.Namespace(ns.String)] = caps.Mask(mask.Int64)
		}

		if isAdmin != 0 {
			// An admin role holds every capability, INCLUDING the global-only ones, so it can
			// only have been granted globally — GrantRole refuses anything else. The
			// belt-and-braces check is here anyway: a hand-edited database row must not be able
			// to turn "Admin on staging" into admin of the fleet.
			if kind != ScopeGlobal {
				continue
			}
			row = caps.Everything
		}

		if kind == ScopeGlobal {
			global = global.Or(row)
			continue
		}
		// A scoped grant is intersected with what may actually be scoped. Even if a global-only
		// bit somehow reached this row, it cannot land in an environment's set.
		if id != "" {
			if env[id] == nil {
				env[id] = caps.Set{}
			}
			env[id] = env[id].Or(row.And(caps.EnvScopable))
		}
	}
	if err := rows.Err(); err != nil {
		return caps.ScopedMask{}, err
	}

	out := caps.ScopedMask{Global: caps.Normalize(global), Env: map[string]caps.Set{}}
	for id, set := range env {
		out.Env[id] = caps.Normalize(set)
	}
	return out, nil
}

// GrantRole adds a membership at a scope. Re-granting an existing one is a no-op rather
// than an error, which is what makes the OIDC re-sync below safe to run on every login.
//
// It REFUSES to scope a role that carries any global-only capability. That is the rule the
// whole model rests on: it is what makes "Admin on staging" unexpressible, and therefore
// what keeps the admin short-circuit above from quietly promoting a scoped grant.
func (s *Store) GrantRole(ctx context.Context, userID, roleID, source string, sc Scope) error {
	if err := s.canGrant(ctx, roleID, sc); err != nil {
		return err
	}

	_, err := s.exec(ctx,
		`INSERT INTO role_members (user_id, role_id, source, scope_kind, scope_id)
         VALUES (?, ?, ?, ?, ?)
         ON CONFLICT (user_id, role_id, scope_kind, scope_id)
         DO UPDATE SET source = excluded.source`,
		userID, roleID, source, sc.Kind, sc.ID)
	if err != nil {
		return fmt.Errorf("store: granting role: %w", err)
	}
	return nil
}

// RevokeRole removes one membership — a role at a particular scope — refusing to strip the
// last administrator.
//
// The scope is part of the identity of the grant: revoking "Operator on staging" must not
// also revoke "Operator on prod".
func (s *Store) RevokeRole(ctx context.Context, userID, roleID string, sc Scope) error {
	r, err := s.RoleByID(ctx, roleID)
	if err != nil {
		return err
	}
	// Only a GLOBAL admin grant can be the last one — a scoped admin grant cannot exist
	// (GrantRole refuses it), so there is no case where revoking a scoped grant strips the
	// fleet's last administrator.
	if r.IsAdmin && sc.IsGlobal() {
		// Would this user still be an admin by some other role? Would anyone else?
		n, err := s.countAdminsExcludingUser(ctx, userID)
		if err != nil {
			return err
		}
		if n == 0 {
			stillAdmin, err := s.hasOtherAdminRole(ctx, userID, roleID)
			if err != nil {
				return err
			}
			if !stillAdmin {
				return ErrLastAdmin
			}
		}
	}
	_, err = s.exec(ctx, `DELETE FROM role_members
        WHERE user_id = ? AND role_id = ? AND scope_kind = ? AND scope_id = ?`,
		userID, roleID, sc.Kind, sc.ID)
	if err != nil {
		return fmt.Errorf("store: revoking role: %w", err)
	}
	return nil
}

func (s *Store) hasOtherAdminRole(ctx context.Context, userID, excludeRole string) (bool, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*)
        FROM role_members rm JOIN roles r ON r.id = rm.role_id
        WHERE rm.user_id = ? AND r.is_admin = 1 AND r.id <> ?`, userID, excludeRole).Scan(&n)
	return n > 0, err
}

// ScopedGrant is a role plus where it applies — what the OIDC re-sync writes.
type ScopedGrant struct {
	RoleID string
	Scope  Scope
}

// SyncOIDCRoles replaces the user's identity-provider-granted roles with exactly these
// grants, and leaves roles granted inside Daffa alone.
//
// That split is the whole point of the source column. The IdP is authoritative for what
// the IdP manages — remove someone from a group there and they lose the role here on their
// next login — but an extra role you handed them in Daffa is not silently wiped by that
// re-sync.
//
// A mapping to a scope that cannot hold that role (a global-only capability on one host, or
// a host that has since been deleted) is SKIPPED, not fatal. A misconfigured mapping must
// not be able to lock out everybody who signs in through that provider — the login proceeds
// with whatever grants are valid, and the bad one is simply not applied.
func (s *Store) SyncOIDCRoles(ctx context.Context, userID string, grants []ScopedGrant) error {
	// Validate before opening the transaction: GrantRole's checks need their own queries,
	// and holding a write tx open across them is exactly the lock-hoarding this codebase
	// avoids elsewhere.
	valid := make([]ScopedGrant, 0, len(grants))
	for _, g := range grants {
		if err := s.canGrant(ctx, g.RoleID, g.Scope); err != nil {
			continue // a broken mapping is ignored, never fatal — see above
		}
		valid = append(valid, g)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: syncing roles: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM role_members WHERE user_id = ? AND source = ?`), userID, SourceOIDC); err != nil {
		return fmt.Errorf("store: clearing provider roles: %w", err)
	}

	for _, g := range valid {
		// A role the user also holds locally keeps its 'local' source: the local grant is
		// the stronger claim, and downgrading it here would let the next re-sync delete a
		// role an administrator granted by hand.
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO role_members (user_id, role_id, source, scope_kind, scope_id)
             VALUES (?, ?, ?, ?, ?)
             ON CONFLICT (user_id, role_id, scope_kind, scope_id) DO NOTHING`),
			userID, g.RoleID, SourceOIDC, g.Scope.Kind, g.Scope.ID); err != nil {
			return fmt.Errorf("store: granting provider role: %w", err)
		}
	}
	return tx.Commit()
}

// canGrant is the shared precondition of GrantRole and SyncOIDCRoles.
func (s *Store) canGrant(ctx context.Context, roleID string, sc Scope) error {
	if !sc.Valid() {
		return errors.New("store: a grant needs either global scope or a host")
	}
	if sc.IsGlobal() {
		return nil
	}
	role, err := s.RoleByID(ctx, roleID)
	if err != nil {
		return err
	}
	if bad := caps.GlobalOnly(role.Effective()); !bad.IsZero() {
		return fmt.Errorf("%w: %q carries %s, which cannot be limited to one host",
			ErrCannotScope, role.Name, strings.Join(bad.Names(), ", "))
	}
	if _, err := s.EnvironmentByID(ctx, sc.ID); err != nil {
		return fmt.Errorf("store: granting a role on an unknown host: %w", err)
	}
	return nil
}

// EnvHasGrants reports whether anybody holds a role scoped to this environment.
//
// It exists for swarm assembly: a node may only be moved out of its environment into the one that
// already IS its swarm when that environment is BARE. An environment somebody has been granted a
// role on is not bare — dissolving it would silently revoke that grant, which is an authorization
// change nobody asked for.
func (s *Store) EnvHasGrants(ctx context.Context, envID string) (bool, error) {
	var n int
	err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM role_members WHERE scope_kind = ? AND scope_id = ?`,
		ScopeEnv, envID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: counting grants on an environment: %w", err)
	}
	return n > 0, nil
}

// RevokeEnvGrants drops every grant scoped to a host. Called when the host is removed —
// otherwise the rows dangle, and re-enrolling an agent that happened to reuse the id would
// silently restore somebody's access.
func (s *Store) RevokeEnvGrants(ctx context.Context, envID string) error {
	_, err := s.exec(ctx,
		`DELETE FROM role_members WHERE scope_kind = ? AND scope_id = ?`, ScopeEnv, envID)
	if err != nil {
		return fmt.Errorf("store: revoking grants on a removed host: %w", err)
	}
	_, err = s.exec(ctx,
		`DELETE FROM oidc_role_mappings WHERE scope_kind = ? AND scope_id = ?`, ScopeEnv, envID)
	if err != nil {
		return fmt.Errorf("store: revoking claim mappings on a removed host: %w", err)
	}
	return nil
}

// ── user guards ─────────────────────────────────────────────────────────────────

// SetUserDisabledGuarded disables a user, refusing to disable the last administrator.
func (s *Store) SetUserDisabledGuarded(ctx context.Context, userID string, disabled bool) error {
	if disabled {
		if err := s.refuseIfLastAdmin(ctx, userID); err != nil {
			return err
		}
	}
	return s.SetUserDisabled(ctx, userID, disabled)
}

// DeleteUserGuarded deletes a user, refusing to delete the last administrator.
func (s *Store) DeleteUserGuarded(ctx context.Context, userID string) error {
	if err := s.refuseIfLastAdmin(ctx, userID); err != nil {
		return err
	}
	return s.DeleteUser(ctx, userID)
}

func (s *Store) refuseIfLastAdmin(ctx context.Context, userID string) error {
	isAdmin, err := s.IsAdminUser(ctx, userID)
	if err != nil {
		return err
	}
	if !isAdmin {
		return nil
	}
	n, err := s.countAdminsExcludingUser(ctx, userID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}
