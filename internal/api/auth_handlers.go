package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// handleAuthConfig tells the login page which methods exist, so it renders a password
// form, one button per identity provider, or both — without the frontend hardcoding a
// deployment's shape.
//
// It is unauthenticated, so it says only what a login page needs: each provider's slug and
// display name. Not its issuer, client id, or claim configuration.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListOIDCProviders(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	type button struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	out := []button{}
	for _, p := range providers {
		if p.Enabled {
			out = append(out, button{Slug: p.Slug, Name: p.Name})
		}
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"local_auth": s.cfg.LocalAuth,
		"providers":  out,
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.LocalAuth {
		httpx.Fail(w, r, http.StatusForbidden, "local_auth_disabled", "Password sign-in is disabled on this server.")
		return
	}

	var req loginRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		httpx.BadRequest(w, r, "Username and password are required.")
		return
	}

	// Throttle per username+IP: neither alone is enough (per-IP punishes a shared
	// egress, per-username lets an attacker rotate IPs).
	key := req.Username + "|" + s.clientIP(r)
	if !s.limiter.Allow(key) {
		s.audit(r.Context(), store.AuditEntry{
			Action: "auth.login", Target: req.Username, Outcome: "denied",
			Detail: store.AuditDetail(map[string]string{"reason": "rate_limited", "ip": s.clientIP(r)}),
		})
		httpx.Fail(w, r, http.StatusTooManyRequests, "rate_limited",
			"Too many failed attempts. Try again later.")
		return
	}

	u, err := s.store.UserByUsername(r.Context(), req.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		httpx.Error(w, r, err)
		return
	}

	// Same failure path whether the user is unknown, disabled, or the password is
	// wrong — and the same work done, so timing does not distinguish them either.
	if err != nil || u.Kind != "local" || u.Disabled {
		_ = auth.VerifyPassword(dummyHash, req.Password)
		s.failLogin(r, key, req.Username, "unknown_or_disabled")
		httpx.Fail(w, r, http.StatusUnauthorized, "invalid_credentials", "Incorrect username or password.")
		return
	}
	if err := auth.VerifyPassword(u.PasswordHash, req.Password); err != nil {
		s.failLogin(r, key, req.Username, "bad_password")
		httpx.Fail(w, r, http.StatusUnauthorized, "invalid_credentials", "Incorrect username or password.")
		return
	}

	s.limiter.Reset(key)
	if err := s.sessions.Issue(r.Context(), w, u.ID, false); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		UserID: u.ID, UserLabel: u.Label(), Action: "auth.login", Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"method": "local", "ip": s.clientIP(r)}),
	})

	// Fresh from the store, so the mask is not attached yet — the middleware that
	// normally does it has not run on this request.
	if u.Caps, err = s.caps.Of(r.Context(), u.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	me, err := s.meResponse(r.Context(), u)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, me)
}

// dummyHash is verified against when the user does not exist, so that a missing user
// and a wrong password cost the same time. It is a hash of a value nobody knows.
var dummyHash = mustHash()

func mustHash() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	h, err := auth.HashPassword(base64.StdEncoding.EncodeToString(b))
	if err != nil {
		// Only reachable if the system CSPRNG is broken, in which case nothing else
		// here is safe either.
		panic("auth: cannot hash: " + err.Error())
	}
	return h
}

func (s *Server) failLogin(r *http.Request, key, username, reason string) {
	s.limiter.Fail(key)
	s.audit(r.Context(), store.AuditEntry{
		Action: "auth.login", Target: username, Outcome: "denied",
		Detail: store.AuditDetail(map[string]string{"reason": reason, "ip": s.clientIP(r)}),
	})
}

// ── OIDC ────────────────────────────────────────────────────────────────────────

// stateStore holds in-flight OIDC authorization requests. They live for minutes and
// die with the process; persisting them would outlive their usefulness.
type stateStore struct {
	mu sync.Mutex
	m  map[string]stateEntry
}

type stateEntry struct {
	verifier string
	// providerID pins the flow to the provider that started it. With several providers
	// configured, a callback that trusted only the URL slug would let a code issued by one
	// IdP be redeemed at another's endpoint.
	providerID string
	expiresAt  time.Time
}

func newStateStore() *stateStore { return &stateStore{m: map[string]stateEntry{}} }

func (s *stateStore) put(state, verifier, providerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Opportunistically drop expired entries; the map is tiny and this saves a goroutine.
	for k, v := range s.m {
		if time.Now().After(v.expiresAt) {
			delete(s.m, k)
		}
	}
	s.m[state] = stateEntry{
		verifier:   verifier,
		providerID: providerID,
		expiresAt:  time.Now().Add(10 * time.Minute),
	}
}

// take consumes a state — an authorization code may be redeemed exactly once.
func (s *stateStore) take(state string) (stateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.m[state]
	delete(s.m, state)
	if !ok || time.Now().After(e.expiresAt) {
		return stateEntry{}, false
	}
	return e, true
}

// ErrNoRoles is what a user gets when their claims match no mapping and their provider has
// no default role. Signing them in with an empty capability mask would render an empty
// application, which reads as a bug; a refusal with a reason reads as a decision.
var errNoRoles = errors.New("this account is not authorized for Daffa")

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	rp, err := s.oidc.BySlug(r.Context(), r.PathValue("provider"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "unknown_provider", "That sign-in method is not available.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	state, err := randomToken()
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	verifier, err := randomToken()
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// The state carries which provider started the flow, so a code minted by one IdP can
	// never be redeemed against another's token endpoint.
	s.oidcStates.put(state, verifier, rp.Provider.ID)

	http.Redirect(w, r, rp.AuthCodeURL(state, verifier), http.StatusFound)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("provider")

	if e := r.URL.Query().Get("error"); e != "" {
		// The IdP rejected the request (consent denied, misconfigured client…).
		// Send the person back to a page that can say so, not to a JSON blob.
		http.Redirect(w, r, "/login?error="+url.QueryEscape(e), http.StatusFound)
		return
	}

	rp, err := s.oidc.BySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "unknown_provider", "That sign-in method is not available.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	st, ok := s.oidcStates.take(r.URL.Query().Get("state"))
	if !ok {
		httpx.Fail(w, r, http.StatusBadRequest, "bad_state",
			"This sign-in request expired or was already used. Start again.")
		return
	}
	// The flow must finish at the provider it started at. Without this, a code obtained
	// from a weak IdP could be presented to this callback for a stronger one.
	if st.providerID != rp.Provider.ID {
		httpx.Fail(w, r, http.StatusBadRequest, "bad_state",
			"This sign-in request was started with a different provider. Start again.")
		return
	}

	id, _, err := rp.Exchange(r.Context(), r.URL.Query().Get("code"), st.verifier)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	u, err := s.resolveOIDCUser(r.Context(), rp, id)
	if errors.Is(err, errNoRoles) {
		s.audit(r.Context(), store.AuditEntry{
			UserLabel: id.Email, Action: "auth.login", Outcome: "denied",
			Detail: store.AuditDetail(map[string]string{
				"reason": "no_mapped_roles", "provider": rp.Provider.Slug, "ip": s.clientIP(r),
			}),
		})
		httpx.Fail(w, r, http.StatusForbidden, "not_authorized",
			"Your account signed in successfully, but it is not assigned any role in Daffa. Ask an administrator to grant you one.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if u.Disabled {
		httpx.Fail(w, r, http.StatusForbidden, "user_disabled", "This account is disabled.")
		return
	}

	if err := s.sessions.Issue(r.Context(), w, u.ID, false); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		UserID: u.ID, UserLabel: u.Label(), Action: "auth.login", Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{
			"method": "oidc", "provider": rp.Provider.Slug, "ip": s.clientIP(r),
		}),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// resolveOIDCUser finds or provisions the local row behind an OIDC identity, and re-syncs
// the roles the provider grants it on every login — so removing someone from a group in
// the IdP takes effect at their next sign-in rather than never.
//
// Two things it deliberately does NOT do:
//
//   - It does not take the most privileged matching role. Claims map to a UNION of roles;
//     capabilities have no ordering for a maximum to be taken over.
//   - It does not touch roles granted inside Daffa. SyncOIDCRoles replaces only the
//     memberships whose source is the provider, so an extra role an admin handed this
//     person by hand survives the re-sync.
func (s *Server) resolveOIDCUser(ctx context.Context, rp *auth.RP, id *auth.Identity) (*store.User, error) {
	p := rp.Provider

	grants, err := s.store.RolesForClaims(ctx, p.ID, rp.ClaimValues(id))
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 && p.DefaultRoleID != "" {
		// The default role is fleet-wide. A default that applied to one host would need a
		// host to name, and "everyone who is not otherwise mapped" is not a per-host idea.
		grants = []store.ScopedGrant{{RoleID: p.DefaultRoleID, Scope: store.Global()}}
	}

	u, err := s.store.UserBySub(ctx, p.ID, id.Sub)

	switch {
	case err == nil:
		// An existing user with no provider-granted roles may still hold local ones —
		// only refuse if they would end up with nothing at all.
		if len(grants) == 0 {
			existing, err := s.store.UserRoles(ctx, u.ID)
			if err != nil {
				return nil, err
			}
			if !hasLocalRole(existing) {
				return nil, errNoRoles
			}
		}
		if err := s.store.SyncOIDCRoles(ctx, u.ID, grants); err != nil {
			return nil, err
		}
		s.caps.Invalidate()

		// The email on the token is the current one; the row may be stale.
		if id.Email != "" && id.Email != u.Email {
			if err := s.store.SetUserEmail(ctx, u.ID, id.Email); err != nil {
				return nil, err
			}
			u.Email = id.Email
		}
		return u, nil

	case errors.Is(err, store.ErrNotFound):
		// A brand-new user with no roles gets nothing to log into. Refuse rather than
		// create an account that renders an empty screen.
		if len(grants) == 0 {
			return nil, errNoRoles
		}

		u := &store.User{Kind: "oidc", Sub: id.Sub, OIDCProvider: p.ID, Email: id.Email}
		if err := s.store.CreateUser(ctx, u); err != nil {
			return nil, err
		}
		if err := s.store.SyncOIDCRoles(ctx, u.ID, grants); err != nil {
			return nil, err
		}
		s.caps.Invalidate()
		return u, nil

	default:
		return nil, err
	}
}

func hasLocalRole(ms []store.Membership) bool {
	for _, m := range ms {
		if m.Source == store.SourceLocal {
			return true
		}
	}
	return false
}

// ── break-glass ─────────────────────────────────────────────────────────────────

// Break-glass tokens are minted by `daffa admin-token` (which requires shell on the
// box — already root-equivalent, since that shell can reach the Docker socket) and
// redeemed exactly once here. The store holds only the hash, and the row is consumed
// on first use, so this is a one-shot recovery path rather than a standing credential.
//
// It exists for the day the IdP is down and the console that manages the IdP's own
// stack is the thing you need to get into.

// MintBreakGlassToken stores a new single-use token and returns the plaintext, which
// the caller prints once and never persists.
func MintBreakGlassToken(ctx context.Context, st *store.Store, ttl time.Duration) (string, error) {
	tok, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := st.CreateBreakGlassToken(ctx, auth.HashToken(tok), time.Now().Add(ttl)); err != nil {
		return "", err
	}
	return tok, nil
}

// handleBreakGlass redeems a one-time admin sign-in token minted by the CLI.
//
// Unlike password login, this path has NO per-IP rate limiter, on purpose. The token is 256-bit
// crypto/rand, hashed at rest, single-use, and consumed on redemption — so online guessing is not
// a threat a throttle would meaningfully reduce. And this is the RECOVERY path: it exists for the
// moment the IdP is down and nobody can get in. A limiter here could lock out the very
// administrator racing to fix that, which is a worse failure than the flood it would prevent.
// Every attempt is audited, and a successful one notifies global recipients, so abuse is loud
// rather than silent. If a throttle is ever added, count only failures and keep it generous.
func (s *Server) handleBreakGlass(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")

	valid := false
	if tok != "" {
		var err error
		valid, err = s.store.RedeemBreakGlassToken(r.Context(), auth.HashToken(tok))
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	if !valid {
		s.audit(r.Context(), store.AuditEntry{
			Action: "auth.break_glass", Outcome: "denied",
			Detail: store.AuditDetail(map[string]string{"ip": s.clientIP(r)}),
		})
		httpx.Fail(w, r, http.StatusForbidden, "invalid_token",
			"This break-glass link is invalid, expired, or has already been used.")
		return
	}

	u, err := s.breakGlassUser(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.sessions.Issue(r.Context(), w, u.ID, true); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Loud on purpose: a break-glass login is an event someone should notice.
	s.audit(r.Context(), store.AuditEntry{
		UserID: u.ID, UserLabel: u.Label(), Action: "auth.break_glass", Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"ip": s.clientIP(r)}),
	})

	// And loud enough to reach somebody who is not looking at the audit log. Somebody just
	// became an administrator by holding a token; if that was not you, you want to know now
	// and not on Monday. Fleet-level, so only GLOBAL recipients are told.
	s.notify.Send(context.WithoutCancel(r.Context()), "", notify.Data{
		Event:   notify.BreakGlassUsed,
		Subject: "Break-glass sign-in used",
		Title:   "Break-glass sign-in used",
		Summary: "Somebody redeemed a break-glass token and signed in as an administrator. " +
			"If this was not expected, treat it as an incident.",
		Detail: "from " + s.clientIP(r),
		Link:   "/audit",
		Failed: true,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// breakGlassUser is a durable local admin row that break-glass sessions attach to, so
// the audit trail has a subject. It cannot be logged into with a password: no hash is
// ever set on it.
//
// It is re-granted the admin role on every use, not just at creation. Break-glass exists
// precisely for the case where the permission model has been damaged — a recovery path
// that could itself be locked out by revoking its role would be no recovery path at all.
func (s *Server) breakGlassUser(ctx context.Context) (*store.User, error) {
	const name = "break-glass"

	u, err := s.store.UserByUsername(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		u = &store.User{Kind: "local", Username: name}
		if err := s.store.CreateUser(ctx, u); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	admin, err := s.store.AdminRole(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.GrantRole(ctx, u.ID, admin.ID, store.SourceLocal, store.Global()); err != nil {
		return nil, err
	}
	s.caps.Invalidate()
	return u, nil
}

// ── session ─────────────────────────────────────────────────────────────────────

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	me, err := s.meResponse(r.Context(), u)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, me)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Revoke(r.Context(), w, r); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, okStatus)
}

// meRole is one role the user holds, and where it applies.
type meRole struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"` // "global" | "env"
	EnvName string `json:"env_name,omitempty"`
}

// meResponse is the UI's whole authorization input.
//
// caps is what the user holds EVERYWHERE; caps_by_env is what they hold on each particular
// host, on top of that. The frontend resolves a question as `global | byEnv[currentHost]`,
// which is why session.can(cap) still takes one argument: a global-only capability can
// never appear in a scoped mask, so the same expression answers both kinds of question.
//
// The masks are plain JSON numbers, which is safe because the registry is capped at bit 52
// and JavaScript integers are exact to 2^53. caps_names is not used for decisions — it is
// there so a person reading the response, or a support conversation, can see what the
// number means without a decoder ring.
type meResponse struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Email   string   `json:"email"`
	Kind    string   `json:"kind"` // local | oidc
	Roles   []meRole `json:"roles"`
	IsAdmin bool     `json:"is_admin"`

	// caps and caps_by_env are OBJECTS — one mask per functional area — rather than the
	// single number they used to be. The browser indexes them by the capability's
	// namespace; see hasCap in web/src/lib/caps.ts.
	Caps      caps.Set            `json:"caps"`
	CapsByEnv map[string]caps.Set `json:"caps_by_env"`
	CapsNames []string            `json:"caps_names"`
}

func (s *Server) meResponse(ctx context.Context, u *store.User) (meResponse, error) {
	roles, err := s.store.UserRoles(ctx, u.ID)
	if err != nil {
		return meResponse{}, err
	}

	rs := make([]meRole, 0, len(roles))
	admin := false
	for _, m := range roles {
		rs = append(rs, meRole{Name: m.Name, Scope: m.Scope.Kind, EnvName: m.EnvName})
		// Only a GLOBAL admin grant makes someone an administrator. A scoped one cannot
		// exist (GrantRole refuses it), but reading it strictly here costs nothing and
		// means a hand-edited row cannot promote anybody.
		admin = admin || (m.IsAdmin && m.Scope.IsGlobal())
	}

	byEnv := make(map[string]caps.Set, len(u.Caps.Env))
	for id, set := range u.Caps.Env {
		byEnv[id] = set
	}

	return meResponse{
		ID:        u.ID,
		Label:     u.Label(),
		Email:     u.Email,
		Kind:      u.Kind,
		Roles:     rs,
		IsAdmin:   admin,
		Caps:      u.Caps.Global,
		CapsByEnv: byEnv,
		CapsNames: u.Caps.Global.Names(),
	}, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// clientIP is the address a login attempt and an audit entry are attributed to. It is
// half the rate-limiter key and the forensic "who", so it must not be forgeable.
//
// X-Forwarded-For is believed ONLY when TrustProxy is set — a client can send that header
// itself, so trusting it unconditionally lets an attacker rotate it to dodge the per-IP login
// throttle and write a false source IP into the audit log. When trusted, the RIGHTMOST entry
// is taken: the reverse proxy appends the address it actually saw, so any values a client
// forged sit to its left and are ignored.
//
// RemoteAddr is host:port, and for IPv6 the host is bracketed ("[::1]:54321") — so it
// has to be split properly, not cut at the first colon.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
