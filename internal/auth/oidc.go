package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/Mnshahawy/daffa/internal/store"
)

// RP is a generic relying party for ONE identity provider. Everything beyond the client
// settings comes from issuer discovery, so it works against any spec-compliant IdP —
// Zitadel, Keycloak, Authentik, Dex, Auth0 — with no provider-specific code.
type RP struct {
	Provider *store.OIDCProvider

	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config

	// endSessionEndpoint is advertised by IdPs that support RP-initiated logout. Absent
	// (Auth0, some others) → we clear our own session and stop there.
	endSessionEndpoint string
}

// Registry builds and caches an RP per provider.
//
// Discovery is a network round trip to the IdP, so doing it per login would put a remote
// service on the critical path of every sign-in. It is done once per provider and dropped
// when the provider is edited — a changed issuer must not keep validating tokens against
// the old one's keys.
type Registry struct {
	store  *store.Store
	sealer interface{ Open(string) (string, error) }

	mu  sync.Mutex
	rps map[string]*RP // by provider id
}

func NewRegistry(s *store.Store, sealer interface{ Open(string) (string, error) }) *Registry {
	return &Registry{store: s, sealer: sealer, rps: map[string]*RP{}}
}

// Forget drops a cached RP. Called whenever a provider is edited or deleted.
func (reg *Registry) Forget(providerID string) {
	reg.mu.Lock()
	delete(reg.rps, providerID)
	reg.mu.Unlock()
}

// For returns the relying party for a provider, building it on first use.
func (reg *Registry) For(ctx context.Context, p *store.OIDCProvider) (*RP, error) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if rp, ok := reg.rps[p.ID]; ok {
		return rp, nil
	}

	secret := ""
	if p.ClientSecretEnc != "" {
		var err error
		secret, err = reg.sealer.Open(p.ClientSecretEnc)
		if err != nil {
			return nil, fmt.Errorf("auth: unsealing the client secret for %q: %w", p.Name, err)
		}
	}

	provider, err := oidc.NewProvider(ctx, p.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: discovery against %s failed: %w", p.Issuer, err)
	}

	var extra struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&extra); err != nil {
		return nil, fmt.Errorf("auth: reading the discovery document: %w", err)
	}

	rp := &RP{
		Provider: p,
		verifier: provider.Verifier(&oidc.Config{ClientID: p.ClientID}),
		oauth: oauth2.Config{
			ClientID:     p.ClientID,
			ClientSecret: secret,
			RedirectURL:  p.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       p.ScopeList(),
		},
		endSessionEndpoint: extra.EndSessionEndpoint,
	}
	reg.rps[p.ID] = rp
	return rp, nil
}

// BySlug resolves an enabled provider and its relying party. Slugs key the callback URL,
// so this is the entry point for both legs of the flow.
func (reg *Registry) BySlug(ctx context.Context, slug string) (*RP, error) {
	p, err := reg.store.OIDCProviderBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, store.ErrNotFound
	}
	return reg.For(ctx, p)
}

// AuthCodeURL starts the flow. PKCE is mandatory: we are a confidential client, but the
// code verifier costs nothing and closes the interception window.
func (rp *RP) AuthCodeURL(state, verifier string) string {
	return rp.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// Identity is what a successful login tells us about the person.
type Identity struct {
	Sub    string
	Email  string
	Name   string
	Claims map[string]any
}

func (rp *RP) Exchange(ctx context.Context, code, verifier string) (*Identity, string, error) {
	tok, err := rp.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, "", fmt.Errorf("auth: code exchange: %w", err)
	}

	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, "", fmt.Errorf("auth: the identity provider returned no id_token")
	}
	idToken, err := rp.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, "", fmt.Errorf("auth: verifying id_token: %w", err)
	}

	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, "", fmt.Errorf("auth: reading id_token claims: %w", err)
	}

	id := &Identity{Sub: idToken.Subject, Claims: claims}
	if v, ok := claims["email"].(string); ok {
		id.Email = v
	}
	if v, ok := claims["name"].(string); ok {
		id.Name = v
	}
	return id, rawID, nil
}

// LogoutURL returns the IdP's RP-initiated logout URL, or "" if it advertises none — in
// which case ending the local session is all we can do.
func (rp *RP) LogoutURL(idToken, postLogoutRedirect string) string {
	if rp.endSessionEndpoint == "" {
		return ""
	}
	u := rp.endSessionEndpoint + "?id_token_hint=" + idToken
	if postLogoutRedirect != "" {
		u += "&post_logout_redirect_uri=" + postLogoutRedirect
	}
	return u
}

// ClaimValues returns the raw values of the provider's roles claim for this identity.
// Mapping them to roles is the store's job (RolesForClaims), because the mappings are
// rows, not configuration.
func (rp *RP) ClaimValues(id *Identity) []string {
	if rp.Provider.RolesClaim == "" {
		return nil
	}
	return claimValues(id.Claims[rp.Provider.RolesClaim])
}

// claimValues flattens the shapes IdPs actually use for role/group claims: a list of
// strings ("groups": ["a","b"]), a single string, or an object whose KEYS are the roles —
// which is what Zitadel's project-roles claim looks like.
func claimValues(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case map[string]any:
		out := make([]string, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(t, &decoded); err != nil {
			return nil
		}
		return claimValues(decoded)
	default:
		return nil
	}
}
