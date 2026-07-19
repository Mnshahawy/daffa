package sshx

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestParseEndpoint(t *testing.T) {
	cases := []struct{ in, wantNet, wantAddr string }{
		{"", "unix", "/var/run/docker.sock"},
		{"unix:///var/run/docker.sock", "unix", "/var/run/docker.sock"},
		{"unix:///run/user/1000/docker.sock", "unix", "/run/user/1000/docker.sock"},
		{"tcp://10.0.0.9:2375", "tcp", "10.0.0.9:2375"},
		{"nonsense", "unix", "/var/run/docker.sock"},
	}
	for _, c := range cases {
		n, a := parseEndpoint(c.in)
		if n != c.wantNet || a != c.wantAddr {
			t.Errorf("parseEndpoint(%q) = (%q,%q); want (%q,%q)", c.in, n, a, c.wantNet, c.wantAddr)
		}
	}
}

// pinningCallback is the whole of host-key TOFU: pin on first use, match afterwards, refuse on
// change. Getting this wrong is the difference between catching a MITM and waving it through.
func TestPinningCallback(t *testing.T) {
	pub := generatedPublicKey(t, "ed25519")
	other := generatedPublicKey(t, "rsa")

	// First use with no pin: trust and record.
	var pinned string
	var mismatch bool
	if err := pinningCallback("", &pinned, &mismatch)("h", nil, pub); err != nil {
		t.Fatalf("first use should trust: %v", err)
	}
	if mismatch || pinned == "" {
		t.Fatalf("first use should pin (mismatch=%v pinned=%q)", mismatch, pinned)
	}

	// Same key against the pin: accepted.
	pinned, mismatch = "", false
	if err := pinningCallback(recordedLine(pub), &pinned, &mismatch)("h", nil, pub); err != nil || mismatch {
		t.Fatalf("matching key should be accepted (err=%v mismatch=%v)", err, mismatch)
	}

	// A DIFFERENT key against the pin: refused, and flagged as a change.
	pinned, mismatch = "", false
	if err := pinningCallback(recordedLine(pub), &pinned, &mismatch)("h", nil, other); err == nil || !mismatch {
		t.Fatalf("changed key must be refused (err=%v mismatch=%v)", err, mismatch)
	}
}

func generatedPublicKey(t *testing.T, algo string) ssh.PublicKey {
	t.Helper()
	km, err := Generate(algo, "test")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(km.PrivatePEM))
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func recordedLine(pub ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
}
