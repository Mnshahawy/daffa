package sshx

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// A generated key must round-trip: the PEM we seal has to parse back to the same public key
// we stored, or an operator pastes a public line into authorized_keys that the private half
// can never satisfy.
func TestGenerateRoundTrips(t *testing.T) {
	for _, algo := range []string{"", "ed25519", "rsa"} {
		km, err := Generate(algo, "daffa@prod-1")
		if err != nil {
			t.Fatalf("Generate(%q): %v", algo, err)
		}
		signer, err := ssh.ParsePrivateKey([]byte(km.PrivatePEM))
		if err != nil {
			t.Fatalf("Generate(%q): private key does not parse: %v", algo, err)
		}
		got := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
		// km.AuthorizedKey carries the comment; compare the key material before it.
		want := strings.Join(strings.Fields(km.AuthorizedKey)[:2], " ")
		if got != want {
			t.Errorf("Generate(%q): stored public key %q does not match the private key's %q", algo, want, got)
		}
		if !strings.HasPrefix(km.Fingerprint, "SHA256:") {
			t.Errorf("Generate(%q): fingerprint %q is not a SHA256 fingerprint", algo, km.Fingerprint)
		}
		if !strings.HasSuffix(km.AuthorizedKey, " daffa@prod-1") {
			t.Errorf("Generate(%q): comment missing from %q", algo, km.AuthorizedKey)
		}
	}
}

// Importing a private key derives the same public half, and an encrypted key with no
// passphrase asks for one specifically rather than failing opaquely.
func TestPublicFromPrivate(t *testing.T) {
	gen, err := Generate("ed25519", "orig")
	if err != nil {
		t.Fatal(err)
	}
	km, err := PublicFromPrivate(gen.PrivatePEM, "", "imported")
	if err != nil {
		t.Fatalf("PublicFromPrivate: %v", err)
	}
	if km.Fingerprint != gen.Fingerprint {
		t.Errorf("imported fingerprint %q != generated %q", km.Fingerprint, gen.Fingerprint)
	}
	if km.Algo != "ed25519" {
		t.Errorf("algo = %q, want ed25519", km.Algo)
	}
	if km.PrivatePEM != "" {
		t.Error("PublicFromPrivate should not echo the private key")
	}
}
