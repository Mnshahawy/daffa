package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OIDCProvider is one identity provider. There may be several — a company IdP for staff
// and a second for contractors, say — so nothing here assumes a singleton.
type OIDCProvider struct {
	ID     string
	Slug   string // keys the callback URL; stable, chosen by the admin
	Name   string // what the login button says
	Issuer string

	ClientID        string
	ClientSecretEnc string // sealed with the master key; never leaves the server

	RedirectURL string
	Scopes      string // space-separated, as in the spec
	RolesClaim  string // e.g. "groups", or Zitadel's project-roles URN

	// DefaultRoleID is what a user gets when their claims match no mapping. NULL means
	// they are REFUSED at login instead — an empty capability mask would render an empty
	// application, which reads as a bug rather than as a decision.
	DefaultRoleID string

	Enabled   bool
	CreatedAt time.Time
}

// ScopeList splits Scopes the way the spec says: space-separated. (This is the same
// gotcha Zitadel integrations hit — comma-separating them silently produces one nonsense
// scope.)
func (p *OIDCProvider) ScopeList() []string { return strings.Fields(p.Scopes) }

// OIDCRoleMapping maps one claim value to one role. Several may match the same user; the
// roles they name are unioned, because capabilities have no ordering to take a maximum of.
type OIDCRoleMapping struct {
	ID         string
	ProviderID string
	ClaimValue string
	RoleID     string
	RoleName   string // joined, for display
	// Scope: a group can map to a role on ONE host — "sre" → Operator on staging. Without
	// this, an SSO-only deployment could not use scoping at all.
	Scope   Scope
	EnvName string // joined, for display; empty for a global mapping
}

const providerCols = `id, slug, name, issuer, client_id, client_secret_enc, redirect_url,
    scopes, roles_claim, default_role_id, enabled, created_at`

func scanProvider(sc interface{ Scan(...any) error }) (*OIDCProvider, error) {
	var p OIDCProvider
	var defaultRole sql.NullString
	var enabled int
	var createdAt string
	if err := sc.Scan(&p.ID, &p.Slug, &p.Name, &p.Issuer, &p.ClientID, &p.ClientSecretEnc,
		&p.RedirectURL, &p.Scopes, &p.RolesClaim, &defaultRole, &enabled, &createdAt); err != nil {
		return nil, err
	}
	p.DefaultRoleID = defaultRole.String
	p.Enabled = enabled != 0
	p.CreatedAt = parseTS(createdAt)
	return &p, nil
}

func (s *Store) ListOIDCProviders(ctx context.Context) ([]*OIDCProvider, error) {
	rows, err := s.query(ctx, `SELECT `+providerCols+` FROM oidc_providers ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: listing identity providers: %w", err)
	}
	defer rows.Close()

	var out []*OIDCProvider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) OIDCProviderByID(ctx context.Context, id string) (*OIDCProvider, error) {
	p, err := scanProvider(s.queryRow(ctx, `SELECT `+providerCols+` FROM oidc_providers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) OIDCProviderBySlug(ctx context.Context, slug string) (*OIDCProvider, error) {
	p, err := scanProvider(s.queryRow(ctx, `SELECT `+providerCols+` FROM oidc_providers WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) CreateOIDCProvider(ctx context.Context, p *OIDCProvider) error {
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now()
	}
	_, err := s.exec(ctx, `INSERT INTO oidc_providers (`+providerCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Slug, p.Name, p.Issuer, p.ClientID, p.ClientSecretEnc, p.RedirectURL,
		p.Scopes, p.RolesClaim, nullStr(p.DefaultRoleID), boolInt(p.Enabled), ts(p.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: creating identity provider: %w", err)
	}
	return nil
}

// UpdateOIDCProvider writes everything except the secret, which has its own method — so
// that an edit form that does not resend the secret cannot blank it.
func (s *Store) UpdateOIDCProvider(ctx context.Context, p *OIDCProvider) error {
	res, err := s.exec(ctx, `UPDATE oidc_providers SET
        slug = ?, name = ?, issuer = ?, client_id = ?, redirect_url = ?,
        scopes = ?, roles_claim = ?, default_role_id = ?, enabled = ?
        WHERE id = ?`,
		p.Slug, p.Name, p.Issuer, p.ClientID, p.RedirectURL, p.Scopes, p.RolesClaim,
		nullStr(p.DefaultRoleID), boolInt(p.Enabled), p.ID)
	if err != nil {
		return fmt.Errorf("store: updating identity provider: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetOIDCProviderSecret(ctx context.Context, id, secretEnc string) error {
	_, err := s.exec(ctx, `UPDATE oidc_providers SET client_secret_enc = ? WHERE id = ?`, secretEnc, id)
	if err != nil {
		return fmt.Errorf("store: updating client secret: %w", err)
	}
	return nil
}

// DeleteOIDCProvider removes a provider. Its role mappings cascade.
//
// Users provisioned by it are NOT deleted: their history in the audit log refers to them,
// and orphaning a person's account is a smaller harm than silently rewriting who did what.
// They simply have no way to sign in until an admin gives them another one.
func (s *Store) DeleteOIDCProvider(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM oidc_providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting identity provider: %w", err)
	}
	return nil
}

// ── role mappings ───────────────────────────────────────────────────────────────

func (s *Store) ListOIDCMappings(ctx context.Context, providerID string) ([]OIDCRoleMapping, error) {
	rows, err := s.query(ctx, `SELECT m.id, m.provider_id, m.claim_value, m.role_id, r.name,
            m.scope_kind, m.scope_id, COALESCE(e.name, '')
        FROM oidc_role_mappings m
        JOIN roles r ON r.id = m.role_id
        LEFT JOIN environments e ON e.id = m.scope_id
        WHERE m.provider_id = ? ORDER BY m.claim_value, r.name`, providerID)
	if err != nil {
		return nil, fmt.Errorf("store: listing role mappings: %w", err)
	}
	defer rows.Close()

	var out []OIDCRoleMapping
	for rows.Next() {
		var m OIDCRoleMapping
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.ClaimValue, &m.RoleID, &m.RoleName,
			&m.Scope.Kind, &m.Scope.ID, &m.EnvName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) CreateOIDCMapping(ctx context.Context, m *OIDCRoleMapping) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.Scope.Kind == "" {
		m.Scope = Global()
	}
	if err := s.canGrant(ctx, m.RoleID, m.Scope); err != nil {
		return err
	}
	_, err := s.exec(ctx, `INSERT INTO oidc_role_mappings
        (id, provider_id, claim_value, role_id, scope_kind, scope_id)
        VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, m.ProviderID, m.ClaimValue, m.RoleID, m.Scope.Kind, m.Scope.ID)
	if err != nil {
		return fmt.Errorf("store: creating role mapping: %w", err)
	}
	return nil
}

func (s *Store) DeleteOIDCMapping(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM oidc_role_mappings WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting role mapping: %w", err)
	}
	return nil
}

// RolesForClaims returns the grants — role AND scope — that every mapping of this provider
// matches, given the claim values a user actually presented.
//
// This is a UNION, not a maximum. The old model ranked roles and took the most privileged
// match, because roles were totally ordered. Capabilities are not ordered — being in both
// "backups" and "deployers" should give you both sets of bits, and there is no meaningful
// sense in which one of them "wins".
//
// The same is true across scopes: a group mapped to Operator on staging AND Operator on
// prod yields both, not one.
func (s *Store) RolesForClaims(ctx context.Context, providerID string, values []string) ([]ScopedGrant, error) {
	if len(values) == 0 {
		return nil, nil
	}

	// Build the IN list by hand: the placeholder count varies, and rebind rewrites ? to
	// $N for Postgres, so the portable move is to generate ?s and let it do its job.
	ph := make([]string, len(values))
	args := make([]any, 0, len(values)+1)
	args = append(args, providerID)
	for i := range values {
		ph[i] = "?"
		args = append(args, values[i])
	}

	rows, err := s.query(ctx, `SELECT DISTINCT role_id, scope_kind, scope_id
        FROM oidc_role_mappings
        WHERE provider_id = ? AND claim_value IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: resolving claim mappings: %w", err)
	}
	defer rows.Close()

	var out []ScopedGrant
	for rows.Next() {
		var g ScopedGrant
		if err := rows.Scan(&g.RoleID, &g.Scope.Kind, &g.Scope.ID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
