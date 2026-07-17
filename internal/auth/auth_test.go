package auth

import (
	"slices"
	"testing"

	"github.com/Mnshahawy/daffa/internal/store"
)

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if err := VerifyPassword(hash, "correct horse battery staple"); err != nil {
		t.Fatalf("VerifyPassword with the right password: %v", err)
	}
	if err := VerifyPassword(hash, "Correct horse battery staple"); err == nil {
		t.Fatal("VerifyPassword accepted a wrong password")
	}

	// Two hashes of the same password must differ — otherwise the salt isn't doing
	// its job and identical passwords are visible as identical rows.
	other, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if other == hash {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}
}

func TestVerifyRejectsGarbageHash(t *testing.T) {
	for _, bad := range []string{"", "plaintext", "$argon2id$broken", "$2y$10$bcryptstyle"} {
		if err := VerifyPassword(bad, "whatever"); err == nil {
			t.Fatalf("VerifyPassword(%q) accepted a malformed hash", bad)
		}
	}
}

// Claim flattening is the seam where a deployment's IdP meets Daffa's permissions, and
// it has to cope with the shapes real IdPs actually emit. Which ROLES those values map to
// is a database question now (store.RolesForClaims); getting the values out of the token
// is this package's job.
func TestClaimValues(t *testing.T) {
	rp := &RP{Provider: &store.OIDCProvider{RolesClaim: "groups"}}

	tests := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{
			name:   "list of strings",
			claims: map[string]any{"groups": []any{"staff", "ops-oncall"}},
			want:   []string{"staff", "ops-oncall"},
		},
		{
			name:   "single string",
			claims: map[string]any{"groups": "ops-admin"},
			want:   []string{"ops-admin"},
		},
		{
			// Zitadel emits project roles as an object keyed by role name.
			name:   "object keyed by role",
			claims: map[string]any{"groups": map[string]any{"ops-admin": map[string]any{"org": "x"}}},
			want:   []string{"ops-admin"},
		},
		{
			name:   "claim absent",
			claims: map[string]any{},
			want:   nil,
		},
		{
			// A number where a group should be is not a group. Skip it rather than
			// stringify it into something that might accidentally match a mapping.
			name:   "non-string members are ignored",
			claims: map[string]any{"groups": []any{"staff", 42}},
			want:   []string{"staff"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rp.ClaimValues(&Identity{Claims: tc.claims})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ClaimValues() = %v; want %v", got, tc.want)
			}
		})
	}
}

// A provider with no roles claim configured must yield nothing — not every value in the
// token. The caller then falls back to the provider's default role, or refuses the login.
func TestClaimValuesWithoutConfiguredClaim(t *testing.T) {
	rp := &RP{Provider: &store.OIDCProvider{}}
	if got := rp.ClaimValues(&Identity{Claims: map[string]any{"groups": []any{"ops-admin"}}}); got != nil {
		t.Fatalf("ClaimValues() with no configured claim = %v; want nil", got)
	}
}
