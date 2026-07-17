package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// Require authenticates the request and attaches the user (and, for bearer requests,
// the token) to its context.
func (m *Manager) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, tok, err := m.Resolve(r)
		if errors.Is(err, ErrNoSession) {
			httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withToken(withUser(r.Context(), u), tok)))
	})
}

// DenialRecorder is how a refused privileged action reaches the audit log. A refused
// attempt to stop a container is exactly the kind of thing an operator wants to find
// there later — recording only the permitted actions would tell you what happened but not
// what someone tried.
type DenialRecorder func(r *http.Request, u *store.User, reason string)

// ScopeOf extracts the environment a request acts on, so the capability can be checked
// where it actually applies. It returns "" for a fleet-wide route.
type ScopeOf func(*http.Request) string

// RequireCap gates a handler on one capability, at the scope the request acts on.
//
// The zero Cap is never satisfied (see caps.Mask.Has), so a route that forgets to declare
// one denies everybody rather than admitting everybody. That is the whole reason a Cap is
// a bit mask and not a bit index.
//
// scope may be nil, which means the route is fleet-wide and only a global grant satisfies
// it. That is the strict reading — a route whose scope extractor was forgotten refuses a
// scoped user rather than admitting them everywhere.
func RequireCap(c caps.Cap, scope ScopeOf, onDenied DenialRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := UserFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}

			env := ""
			if scope != nil {
				env = scope(r)
			}
			if !u.Caps.Has(c, env) {
				Deny(w, r, u, c, onDenied)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireCapAnywhere gates a handler on a capability held globally OR on any host.
//
// It is for the three fleet-wide read lists — git credentials, registries, storage targets —
// which have no environment of their own, but which an operator scoped to one host still
// has to be able to see in order to pick one when creating a stack. Their responses carry
// names and kinds, never secrets, which is what makes the widening safe.
func RequireCapAnywhere(c caps.Cap, onDenied DenialRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := UserFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}
			if !u.Caps.HasAnywhere(c) {
				Deny(w, r, u, c, onDenied)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Deny refuses an action and audits it. Handlers that must check a capability themselves —
// exec, because a WebSocket upgrade is a GET and cannot sit behind a method-specific guard;
// and the two creates that carry their environment in the request BODY, where no middleware
// can see it — call this so their denials look identical to every other one in the log.
func Deny(w http.ResponseWriter, r *http.Request, u *store.User, c caps.Cap, onDenied DenialRecorder) {
	if onDenied != nil {
		onDenied(r, u, "missing_capability:"+c.Name())
	}
	httpx.Fail(w, r, http.StatusForbidden, "forbidden",
		"You do not have the "+c.Name()+" permission here.")
}

// CSRF rejects state-changing requests that did not originate from our own page.
// The SPA is same-origin by construction and we send no CORS headers, so an Origin
// (or Sec-Fetch-Site) check plus the SameSite=Strict cookie is sufficient — there is
// no token to synchronize.
func CSRF(selfOrigin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}

			// Browsers that send Sec-Fetch-Site give us the cleanest signal.
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
				if site != "same-origin" && site != "none" {
					httpx.Fail(w, r, http.StatusForbidden, "csrf", "Cross-site request rejected.")
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			// Otherwise fall back to Origin. A missing Origin on a same-origin XHR is
			// legal in older browsers; a *mismatched* one never is.
			//
			// Allowing "no Sec-Fetch-Site AND no Origin" through looks like a hole but is not the
			// last line of defence: the session cookie is SameSite=Strict + __Host- (see
			// session.go), so a browser will not attach it to ANY cross-site request in the first
			// place — and every browser new enough to be a CSRF concern sends Sec-Fetch-Site, so a
			// real cross-site attack is caught by the branch above and never reaches this fallback.
			// This case is the genuinely-old same-origin XHR. Do not tighten it into a rejection
			// without a reason to drop those clients; the security does not depend on it.
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !originMatches(origin, selfOrigin, r) {
				httpx.Fail(w, r, http.StatusForbidden, "csrf", "Cross-site request rejected.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func originMatches(origin, selfOrigin string, r *http.Request) bool {
	if selfOrigin != "" && strings.EqualFold(origin, selfOrigin) {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// Compare against the Host we were actually reached on, which is what a proxy
	// (Traefik) preserves and what the browser used.
	return strings.EqualFold(u.Host, r.Host)
}
