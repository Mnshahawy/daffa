package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The cookie holds a random token; the store holds only its SHA-256. A dump of the
// sessions table therefore cannot be replayed as a login.
const (
	CookieName         = "__Host-daffa"
	cookieNameInsecure = "daffa_session" // __Host- requires HTTPS; dev over http:// cannot use it
)

type ctxKey int

const (
	userKey  ctxKey = 1
	tokenKey ctxKey = 2
)

// TokenPrefix opens every API token. It exists for secret scanners and for the human
// reading a leaked CI log — the entropy behind it is the security.
const TokenPrefix = "daffa_"

// NewAPIToken mints a bearer secret: the prefix plus 32 random bytes. Returned once,
// stored only as its hash.
func NewAPIToken() (string, error) {
	t, err := newToken()
	if err != nil {
		return "", err
	}
	return TokenPrefix + t, nil
}

// Manager issues and resolves sessions.
type Manager struct {
	store  *store.Store
	caps   *caps.Cache
	ttl    time.Duration
	secure bool // false only for local http:// development
}

func NewManager(s *store.Store, c *caps.Cache, ttl time.Duration, secure bool) *Manager {
	return &Manager{store: s, caps: c, ttl: ttl, secure: secure}
}

func (m *Manager) cookieName() string {
	if m.secure {
		return CookieName
	}
	return cookieNameInsecure
}

// HashToken is what gets stored for anything bearer-shaped (session cookies,
// break-glass tokens): the store never holds a value that could be replayed.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generating session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Issue creates a session for the user and sets the cookie.
func (m *Manager) Issue(ctx context.Context, w http.ResponseWriter, userID string, breakGlass bool) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(m.ttl)
	if breakGlass {
		// A break-glass session is for getting back in and fixing auth, not for
		// living in.
		expires = time.Now().Add(1 * time.Hour)
	}

	if err := m.store.CreateSession(ctx, &store.Session{
		ID:         HashToken(token),
		UserID:     userID,
		BreakGlass: breakGlass,
		ExpiresAt:  expires,
	}); err != nil {
		return err
	}
	if err := m.store.TouchLogin(ctx, userID); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

// Revoke deletes the caller's session and clears the cookie.
func (m *Manager) Revoke(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(m.cookieName()); err == nil {
		if err := m.store.DeleteSession(ctx, HashToken(c.Value)); err != nil {
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

// Resolve returns the user behind the request — a session cookie, or a bearer API
// token — or ErrNoSession. The non-nil *store.APIToken says which credential it was:
// token requests are barred from a small set of self-rekeying actions (docs/tokens.md),
// and their audit entries carry the attribution.
var ErrNoSession = errors.New("auth: no session")

func (m *Manager) Resolve(r *http.Request) (*store.User, *store.APIToken, error) {
	// A bearer header wins over a cookie: it is an explicit statement of which
	// credential the caller means, and a browser never sends one.
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer "+TokenPrefix) {
		return m.resolveToken(r, strings.TrimPrefix(h, "Bearer "))
	}

	c, err := r.Cookie(m.cookieName())
	if err != nil || c.Value == "" {
		return nil, nil, ErrNoSession
	}
	u, _, err := m.store.SessionUser(r.Context(), HashToken(c.Value))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNoSession
	}
	if err != nil {
		return nil, nil, err
	}
	if err := m.attachCaps(r, u); err != nil {
		return nil, nil, err
	}
	return u, nil, nil
}

func (m *Manager) resolveToken(r *http.Request, secret string) (*store.User, *store.APIToken, error) {
	u, tok, err := m.store.APITokenUser(r.Context(), HashToken(secret))
	if errors.Is(err, store.ErrNotFound) {
		// Unknown, expired, or a disabled owner — one refusal. The distinctions live in
		// the tokens list, for someone signed in; an unauthenticated probe learns nothing.
		return nil, nil, ErrNoSession
	}
	if err != nil {
		return nil, nil, err
	}
	if err := m.attachCaps(r, u); err != nil {
		return nil, nil, err
	}
	// Record use, throttled: "when was this last used" does not need per-request
	// precision, and busy CI must not turn every call into a write.
	if time.Since(tok.LastUsedAt) > time.Minute {
		if err := m.store.TouchAPIToken(r.Context(), tok.ID); err == nil {
			tok.LastUsedAt = time.Now()
		}
	}
	return u, tok, nil
}

// attachCaps loads the capability mask, cached, so every downstream check is a bit test.
// An error is returned rather than swallowed into an empty mask: "the database is
// unreachable" and "this user may do nothing" both end in a refusal, but only one of
// them should be logged as a permissions failure.
func (m *Manager) attachCaps(r *http.Request, u *store.User) error {
	mask, err := m.caps.Of(r.Context(), u.ID)
	if err != nil {
		return err
	}
	u.Caps = mask
	return nil
}

// UserFrom returns the authenticated user attached by Middleware.
func UserFrom(ctx context.Context) (*store.User, bool) {
	u, ok := ctx.Value(userKey).(*store.User)
	return u, ok
}

func withUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// TokenFrom returns the API token the request authenticated with, when it did — the
// signal for the two things a token may never do (mint tokens, change passwords) and
// for audit attribution.
func TokenFrom(ctx context.Context) (*store.APIToken, bool) {
	t, ok := ctx.Value(tokenKey).(*store.APIToken)
	return t, ok
}

func withToken(ctx context.Context, t *store.APIToken) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, tokenKey, t)
}
