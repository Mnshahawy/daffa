package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// A slug ends up in a URL that the identity provider must be told about verbatim, so it is
// constrained to the characters that survive being pasted into one.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)

type providerView struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	Issuer        string `json:"issuer"`
	ClientID      string `json:"client_id"`
	RedirectURL   string `json:"redirect_url"`
	Scopes        string `json:"scopes"`
	RolesClaim    string `json:"roles_claim"`
	DefaultRoleID string `json:"default_role_id"`
	Enabled       bool   `json:"enabled"`

	// HasSecret, never the secret. It is sealed with the master key and there is no
	// endpoint that reads it back — an admin who has lost it replaces it.
	HasSecret bool `json:"has_secret"`
}

func viewProvider(p *store.OIDCProvider) providerView {
	return providerView{
		ID: p.ID, Slug: p.Slug, Name: p.Name, Issuer: p.Issuer,
		ClientID: p.ClientID, RedirectURL: p.RedirectURL, Scopes: p.Scopes,
		RolesClaim: p.RolesClaim, DefaultRoleID: p.DefaultRoleID, Enabled: p.Enabled,
		HasSecret: p.ClientSecretEnc != "",
	}
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListOIDCProviders(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]providerView, 0, len(ps))
	for _, p := range ps {
		out = append(out, viewProvider(p))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type providerRequest struct {
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	Issuer        string `json:"issuer"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"` // "" on update ⇒ leave the stored one alone
	RedirectURL   string `json:"redirect_url"`
	Scopes        string `json:"scopes"`
	RolesClaim    string `json:"roles_claim"`
	DefaultRoleID string `json:"default_role_id"`
	Enabled       bool   `json:"enabled"`
}

func (rq *providerRequest) validate() string {
	rq.Slug = strings.TrimSpace(strings.ToLower(rq.Slug))
	rq.Name = strings.TrimSpace(rq.Name)
	rq.Issuer = strings.TrimRight(strings.TrimSpace(rq.Issuer), "/")

	switch {
	case !slugRe.MatchString(rq.Slug):
		return "The slug must be lowercase letters, numbers and hyphens — it appears in the sign-in URL."
	case rq.Name == "":
		return "Give the provider a name. It is what the sign-in button says."
	case !strings.HasPrefix(rq.Issuer, "https://") && !strings.HasPrefix(rq.Issuer, "http://"):
		return "The issuer must be a URL, e.g. https://auth.example.com."
	case rq.ClientID == "":
		return "A client ID is required."
	case rq.RedirectURL == "":
		return "A redirect URL is required. Register the same value with the provider."
	}
	if strings.TrimSpace(rq.Scopes) == "" {
		rq.Scopes = "openid profile email"
	}
	// Scopes are SPACE-separated per the spec. Commas silently produce one nonsense scope
	// that most IdPs then reject with an unhelpful error, so fix it here rather than let
	// someone spend an afternoon on it.
	rq.Scopes = strings.Join(strings.FieldsFunc(rq.Scopes, func(c rune) bool {
		return c == ',' || c == ' ' || c == '\t'
	}), " ")
	return ""
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req providerRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		httpx.BadRequest(w, r, msg)
		return
	}

	p := &store.OIDCProvider{
		Slug: req.Slug, Name: req.Name, Issuer: req.Issuer, ClientID: req.ClientID,
		RedirectURL: req.RedirectURL, Scopes: req.Scopes, RolesClaim: req.RolesClaim,
		DefaultRoleID: req.DefaultRoleID, Enabled: req.Enabled,
	}
	if req.ClientSecret != "" {
		sealed, err := s.sealer.Seal(req.ClientSecret)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		p.ClientSecretEnc = sealed
	}

	if err := s.store.CreateOIDCProvider(r.Context(), p); err != nil {
		if store.IsDuplicate(err) {
			httpx.Fail(w, r, http.StatusConflict, "duplicate_slug", "A provider with that slug already exists.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	s.auditProvider(r, "oidc.create", p)
	httpx.JSON(w, http.StatusCreated, viewProvider(p))
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.OIDCProviderByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "That provider does not exist.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req providerRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		httpx.BadRequest(w, r, msg)
		return
	}

	p.Slug, p.Name, p.Issuer = req.Slug, req.Name, req.Issuer
	p.ClientID, p.RedirectURL, p.Scopes = req.ClientID, req.RedirectURL, req.Scopes
	p.RolesClaim, p.DefaultRoleID, p.Enabled = req.RolesClaim, req.DefaultRoleID, req.Enabled

	if err := s.store.UpdateOIDCProvider(r.Context(), p); err != nil {
		if store.IsDuplicate(err) {
			httpx.Fail(w, r, http.StatusConflict, "duplicate_slug", "A provider with that slug already exists.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	// An empty secret means "leave it alone", so an edit form that does not resend it
	// cannot blank it by omission.
	if req.ClientSecret != "" {
		sealed, err := s.sealer.Seal(req.ClientSecret)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if err := s.store.SetOIDCProviderSecret(r.Context(), p.ID, sealed); err != nil {
			httpx.Error(w, r, err)
			return
		}
		p.ClientSecretEnc = sealed
	}

	// Drop the cached relying party: the issuer may have changed, and continuing to
	// validate tokens against the previous one's keys is exactly the bug you would not
	// find until someone could not log in.
	s.oidc.Forget(p.ID)

	s.auditProvider(r, "oidc.update", p)
	httpx.JSON(w, http.StatusOK, viewProvider(p))
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.OIDCProviderByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "That provider does not exist.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := s.store.DeleteOIDCProvider(r.Context(), p.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.oidc.Forget(p.ID)

	s.auditProvider(r, "oidc.delete", p)
	httpx.JSON(w, http.StatusOK, okStatus)
}

// handleTestProvider fetches the provider's discovery document. It is the cheap version of
// "does this actually work", and it turns the most common misconfiguration — a typo'd
// issuer — into an error message on the settings page rather than a failed login for
// somebody else an hour later.
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.OIDCProviderByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "That provider does not exist.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Build it fresh rather than take the cached one, so "Test" tests what is stored now.
	s.oidc.Forget(p.ID)
	if _, err := s.oidc.For(r.Context(), p); err != nil {
		httpx.JSON(w, http.StatusOK, testResult{OK: false, Error: err.Error()})
		return
	}
	httpx.JSON(w, http.StatusOK, testResult{
		OK:      true,
		Message: "Discovery succeeded. " + p.Issuer + " answered and its signing keys were readable.",
	})
}

// ── claim → role mappings ───────────────────────────────────────────────────────

func (s *Server) handleListMappings(w http.ResponseWriter, r *http.Request) {
	ms, err := s.store.ListOIDCMappings(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if ms == nil {
		ms = []store.OIDCRoleMapping{}
	}
	httpx.JSON(w, http.StatusOK, ms)
}

type mappingRequest struct {
	ClaimValue string `json:"claim_value"`
	RoleID     string `json:"role_id"`
	// EnvID empty ⇒ the role is granted everywhere. A role that administers Daffa itself
	// cannot be limited to one host, and the store refuses it.
	EnvID string `json:"env_id"`
}

func (s *Server) handleCreateMapping(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")

	var req mappingRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.ClaimValue = strings.TrimSpace(req.ClaimValue)
	if req.ClaimValue == "" || req.RoleID == "" {
		httpx.BadRequest(w, r, "A mapping needs a claim value and a role.")
		return
	}

	sc := store.Global()
	if req.EnvID != "" {
		sc = store.OnEnv(req.EnvID)
	}

	m := &store.OIDCRoleMapping{
		ProviderID: providerID, ClaimValue: req.ClaimValue, RoleID: req.RoleID, Scope: sc,
	}
	if err := s.store.CreateOIDCMapping(r.Context(), m); err != nil {
		if store.IsDuplicate(err) {
			httpx.Fail(w, r, http.StatusConflict, "duplicate_mapping",
				"That claim already maps to that role, at that scope.")
			return
		}
		if errors.Is(err, store.ErrCannotScope) {
			httpx.Fail(w, r, http.StatusConflict, "cannot_scope", err.Error())
			return
		}
		httpx.Error(w, r, err)
		return
	}

	// Mappings take effect at each user's next sign-in, when their provider roles are
	// re-synced. Nothing cached needs dropping here.
	s.auditMapping(r, "oidc.map", m)
	httpx.JSON(w, http.StatusCreated, m)
}

func (s *Server) handleDeleteMapping(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteOIDCMapping(r.Context(), r.PathValue("mapping")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.auditMapping(r, "oidc.unmap", &store.OIDCRoleMapping{ID: r.PathValue("mapping")})
	httpx.JSON(w, http.StatusOK, okStatus)
}

func (s *Server) auditProvider(r *http.Request, action string, p *store.OIDCProvider) {
	u, _ := auth.UserFrom(r.Context())
	e := store.AuditEntry{Action: action, Target: p.Slug, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"issuer": p.Issuer, "enabled": p.Enabled})}
	if u != nil {
		e.UserID, e.UserLabel = u.ID, u.Label()
	}
	s.audit(r.Context(), e)
}

func (s *Server) auditMapping(r *http.Request, action string, m *store.OIDCRoleMapping) {
	u, _ := auth.UserFrom(r.Context())
	e := store.AuditEntry{Action: action, Target: m.ClaimValue, Outcome: "ok"}
	if u != nil {
		e.UserID, e.UserLabel = u.ID, u.Label()
	}
	s.audit(r.Context(), e)
}
