package stacks

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The CABundle on a git Source must actually change TLS trust — the whole point of the
// internal-CA fix. An httptest TLS server presents a cert signed by an authority the system
// roots do not know, so:
//
//   - without the bundle, the clone fails at TLS verification (a certificate error);
//   - with the server's cert in the bundle, TLS passes and the clone fails LATER, for a
//     non-certificate reason (the endpoint is not a real git server).
//
// TLS is verified before any git protocol bytes flow, so a plain 404 handler is enough — we
// only care WHICH error comes back.
func TestCloneCommitHonorsCABundle(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	src := Source{Kind: "git", URL: srv.URL}

	// Without the CA: TLS verification fails, and friendlyGitError passes an x509 error through
	// untouched, so the message still names the certificate problem.
	_, err := cloneCommit(context.Background(), src)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected a certificate-trust failure without the CA bundle, got: %v", err)
	}

	// With the server's own cert in the bundle, TLS now verifies. The clone still fails (this is
	// not a git server), but the error must no longer be about the certificate.
	src.CABundle = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	_, err = cloneCommit(context.Background(), src)
	if err == nil {
		t.Fatal("clone unexpectedly succeeded against a non-git endpoint")
	}
	if strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the CA bundle was not honoured — TLS still failed: %v", err)
	}
}

// CheckAccess (the ls-remote credential test) must honour the same CABundle — it is the whole
// point of testing an internal-CA git server. Same signal as the clone test: a certificate error
// without the bundle, a different (non-certificate) failure with it.
func TestCheckAccessHonorsCABundle(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	src := Source{Kind: "git", URL: srv.URL}
	if err := CheckAccess(context.Background(), src); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected a certificate-trust failure without the CA bundle, got: %v", err)
	}

	src.CABundle = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := CheckAccess(context.Background(), src); err == nil {
		t.Fatal("CheckAccess unexpectedly succeeded against a non-git endpoint")
	} else if strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the CA bundle was not honoured — TLS still failed: %v", err)
	}
}
