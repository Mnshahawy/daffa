package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// API tokens are a way to BE a user without a session. This covers the resolve path and the
// three ways a token stops resolving without its row vanishing: it expires, its user is
// disabled, or its user is deleted (which does take the row, by cascade).
func TestAPITokens(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		u := &User{Kind: "local", Username: "ci-deploy"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}

		tok := &APIToken{UserID: u.ID, Name: "forgejo", Prefix: "daffa_3fJk", Hash: "hash-a"}
		if err := s.CreateAPIToken(ctx, tok); err != nil {
			t.Fatal(err)
		}

		// The resolve path: hash → enabled user + token.
		ru, rt, err := s.APITokenUser(ctx, "hash-a")
		if err != nil || ru.ID != u.ID || rt.ID != tok.ID {
			t.Fatalf("APITokenUser = %v, %v, %v; want the ci user and its token", ru, rt, err)
		}

		// One name per user: "revoke the forgejo token" must never be ambiguous.
		dup := &APIToken{UserID: u.ID, Name: "forgejo", Prefix: "daffa_9xYz", Hash: "hash-b"}
		if err := s.CreateAPIToken(ctx, dup); !IsDuplicate(err) {
			t.Fatalf("a second token named forgejo under one user = %v; want a duplicate refusal", err)
		}

		// An expired token is refused at resolve but its row survives — the answer to
		// "what broke CI last night" must not delete itself.
		exp := &APIToken{UserID: u.ID, Name: "old", Prefix: "daffa_old", Hash: "hash-c",
			ExpiresAt: now().Add(-time.Hour)}
		if err := s.CreateAPIToken(ctx, exp); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.APITokenUser(ctx, "hash-c"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an expired token resolved: %v", err)
		}
		if _, err := s.APITokenByID(ctx, exp.ID); err != nil {
			t.Errorf("the expired token's row must survive: %v", err)
		}

		// Disabling the user is the kill switch for every token they own.
		if err := s.SetUserDisabled(ctx, u.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.APITokenUser(ctx, "hash-a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a disabled user's token resolved: %v", err)
		}
		if err := s.SetUserDisabled(ctx, u.ID, false); err != nil {
			t.Fatal(err)
		}

		// Deleting the user cascades to the tokens.
		if err := s.DeleteUser(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.AllAPITokens(ctx); len(got) != 0 {
			t.Errorf("tokens survived their user's deletion: %d rows", len(got))
		}
	})
}
