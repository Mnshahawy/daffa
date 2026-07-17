package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// APIToken is a bearer credential that makes its owner's user reachable without a
// session. The secret is never here: ID names the row, Hash verifies the bearer, Prefix
// is what a human sees when the UI has to say which token this is. See docs/tokens.md.
type APIToken struct {
	ID         string
	UserID     string
	Name       string
	Prefix     string
	Hash       string
	ExpiresAt  time.Time // zero = does not expire
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// Expired reports whether the token has an expiry and is past it.
func (t *APIToken) Expired() bool {
	return !t.ExpiresAt.IsZero() && t.ExpiresAt.Before(now())
}

const tokenCols = `id, user_id, name, prefix, hash, expires_at, created_at, last_used_at`

func scanToken(sc interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var expires, lastUsed sql.NullString
	var createdAt string
	err := sc.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Hash, &expires, &createdAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if expires.Valid {
		t.ExpiresAt = parseTS(expires.String)
	}
	t.CreatedAt = parseTS(createdAt)
	if lastUsed.Valid {
		t.LastUsedAt = parseTS(lastUsed.String)
	}
	return &t, nil
}

func (s *Store) CreateAPIToken(ctx context.Context, t *APIToken) error {
	if t.ID == "" {
		t.ID = "tok_" + NewID()
	}
	t.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO api_tokens (`+tokenCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, t.Prefix, t.Hash, nullTS(t.ExpiresAt), ts(t.CreatedAt), nullTS(t.LastUsedAt))
	if err != nil {
		return fmt.Errorf("store: creating api token: %w", err)
	}
	return nil
}

func (s *Store) APITokenByID(ctx context.Context, id string) (*APIToken, error) {
	return scanToken(s.queryRow(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE id = ?`, id))
}

// APITokenUser resolves a bearer secret's hash to its token and its (enabled) user —
// SessionUser's shape for the header-shaped credential. An expired token or a disabled
// user is ErrNotFound: to the caller both mean "this credential no longer admits anyone",
// and the row survives so the UI can still say WHY.
func (s *Store) APITokenUser(ctx context.Context, hash string) (*User, *APIToken, error) {
	row := s.queryRow(ctx, `SELECT `+prefixed("t", tokenCols)+`, `+
		`u.id, u.kind, u.username, u.password_hash, u.sub, u.oidc_provider_id, u.email, u.disabled, u.created_at, u.last_login_at `+
		`FROM api_tokens t JOIN users u ON u.id = t.user_id WHERE t.hash = ?`, hash)

	var t APIToken
	var expires, lastUsed sql.NullString
	var tCreated string
	var u User
	var username, phash, sub, provider, lastLogin sql.NullString
	var uCreated string
	var disabled int
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Hash, &expires, &tCreated, &lastUsed,
		&u.ID, &u.Kind, &username, &phash, &sub, &provider, &u.Email, &disabled, &uCreated, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	if expires.Valid {
		t.ExpiresAt = parseTS(expires.String)
	}
	t.CreatedAt = parseTS(tCreated)
	if lastUsed.Valid {
		t.LastUsedAt = parseTS(lastUsed.String)
	}
	if t.Expired() || disabled != 0 {
		return nil, nil, ErrNotFound
	}

	u.Username = username.String
	u.PasswordHash = phash.String
	u.Sub = sub.String
	u.OIDCProvider = provider.String
	u.Disabled = disabled != 0
	u.CreatedAt = parseTS(uCreated)
	if lastLogin.Valid {
		u.LastLoginAt = parseTS(lastLogin.String)
	}
	return &u, &t, nil
}

// TouchAPIToken records use. Callers throttle it (once a minute per token) — "when was
// this last used" does not need per-request precision, and busy CI must not turn every
// API call into a write.
func (s *Store) TouchAPIToken(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, ts(now()), id)
	return err
}

// ListAPITokens returns one user's tokens, newest first.
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error) {
	return s.listTokens(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`, userID)
}

// AllAPITokens is the users.edit oversight list: every token, every owner.
func (s *Store) AllAPITokens(ctx context.Context) ([]*APIToken, error) {
	return s.listTokens(ctx, `SELECT `+tokenCols+` FROM api_tokens ORDER BY created_at DESC`)
}

func (s *Store) listTokens(ctx context.Context, query string, args ...any) ([]*APIToken, error) {
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing api tokens: %w", err)
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIToken(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	return err
}

// prefixed qualifies each column in a comma-separated list with a table alias, so a join
// can reuse the canonical column list instead of restating it.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
