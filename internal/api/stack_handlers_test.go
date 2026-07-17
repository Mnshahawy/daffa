package api

import "testing"

// A secret name becomes daffa-secrets/<name> in the bundle and /run/secrets/<name> in the
// container, so anything that could escape that directory must be refused before it is written.
func TestValidSecretName(t *testing.T) {
	ok := []string{"db_password", "tls.key", "GOOGLE_CREDS", "a-b.c_d", "x"}
	for _, n := range ok {
		if !validSecretName(n) {
			t.Errorf("%q should be a valid secret name", n)
		}
	}
	bad := []string{"", "a/b", "..", "../escape", "a..b", `a\b`, "with space", "emoji😀", "a/../b"}
	for _, n := range bad {
		if validSecretName(n) {
			t.Errorf("%q must be refused as a secret name", n)
		}
	}
}
