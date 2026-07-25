package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mnshahawy/daffa/internal/store"
)

// An admin with users.edit can change a LOCAL account's email — the gap this feature closed.
// The route guard is covered elsewhere; this exercises the handler's own rules: a valid
// address round-trips, junk is refused, and an SSO account's email is off-limits because the
// provider owns it and re-syncs it on every sign-in.
func TestUpdateUserEmail(t *testing.T) {
	s, ctx := certServer(t)

	local := &store.User{Kind: "local", Username: "operator", Email: "old@example.com"}
	if err := s.store.CreateUser(ctx, local); err != nil {
		t.Fatal(err)
	}

	// A valid change round-trips through the store and comes back on the view.
	rec := call(s.handleUpdateUser, "PATCH", "/api/users/"+local.ID,
		map[string]string{"id": local.ID}, `{"email":"new@example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid email edit returned %d: %s", rec.Code, rec.Body.String())
	}
	var view userView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Email != "new@example.com" {
		t.Errorf("email not updated on the view: %q", view.Email)
	}
	if got, _ := s.store.UserByID(ctx, local.ID); got.Email != "new@example.com" {
		t.Errorf("email not persisted: %q", got.Email)
	}

	// The edit is audited — a mutation that changes who an account belongs to must leave a
	// trace, like every other user mutation.
	entries, err := s.store.ListAudit(ctx, 20, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAuditAction(entries, "user.email") {
		t.Error("changing an email left no audit entry")
	}

	// Junk is refused before it reaches the store.
	rec = call(s.handleUpdateUser, "PATCH", "/api/users/"+local.ID,
		map[string]string{"id": local.ID}, `{"email":"not an address"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid email returned %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Clearing is legitimate — a local account is identified by its username.
	rec = call(s.handleUpdateUser, "PATCH", "/api/users/"+local.ID,
		map[string]string{"id": local.ID}, `{"email":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clearing the email returned %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.store.UserByID(ctx, local.ID); got.Email != "" {
		t.Errorf("email not cleared: %q", got.Email)
	}

	// An SSO account's email is the provider's, re-synced on every sign-in. Editing it here
	// would silently revert, so the handler refuses rather than half-work.
	prov := &store.OIDCProvider{Slug: "corp", Name: "Corp SSO", Issuer: "https://issuer.example.com",
		ClientID: "cid", ClientSecretEnc: "sealed"}
	if err := s.store.CreateOIDCProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	sso := &store.User{Kind: "oidc", Sub: "sub-1", OIDCProvider: prov.ID, Email: "sso@example.com"}
	if err := s.store.CreateUser(ctx, sso); err != nil {
		t.Fatal(err)
	}
	rec = call(s.handleUpdateUser, "PATCH", "/api/users/"+sso.ID,
		map[string]string{"id": sso.ID}, `{"email":"changed@example.com"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("editing an SSO email returned %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.store.UserByID(ctx, sso.ID); got.Email != "sso@example.com" {
		t.Errorf("an SSO account's email was changed: %q", got.Email)
	}
}

func hasAuditAction(entries []*store.AuditEntry, action string) bool {
	for _, e := range entries {
		if e.Action == action {
			return true
		}
	}
	return false
}
