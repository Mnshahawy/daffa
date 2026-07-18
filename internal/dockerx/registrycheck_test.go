package dockerx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The WWW-Authenticate parser is the fiddly part: a realm URL can carry its own query string with
// commas in it, so a naive split-on-comma truncates the token endpoint and the whole check then
// authenticates against the wrong URL.
func TestParseChallengeKeepsCommasInsideQuotes(t *testing.T) {
	h := `Bearer realm="https://auth.docker.io/token?a=1,b=2",service="registry.docker.io",scope="repository:x:pull"`
	got := parseChallenge(h)

	if got["realm"] != "https://auth.docker.io/token?a=1,b=2" {
		t.Errorf("realm was truncated at an in-value comma: %q", got["realm"])
	}
	if got["service"] != "registry.docker.io" {
		t.Errorf("service = %q", got["service"])
	}
}

func TestRegistryBaseURLNormalisesDockerHub(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io":              "https://registry-1.docker.io",
		"ghcr.io":                "https://ghcr.io",
		"https://quay.io":        "https://quay.io",
		"registry.local/":        "https://registry.local",
		"http://forgejo:3000":    "http://forgejo:3000",   // an explicit http:// is honoured
		"http://registry.local/": "http://registry.local", // …and the trailing slash still trimmed
	} {
		if got := registryBaseURL(in); got != want {
			t.Errorf("registryBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The Bearer flow against a stub registry: /v2/ challenges, the token endpoint checks the
// credentials, and only a token that actually comes back counts as success.
func TestCheckRegistryBearerFlow(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token":
			// The stub accepts exactly one credential.
			user, pass, _ := r.BasicAuth()
			if user == "good" && pass == "pw" {
				_, _ = w.Write([]byte(`{"token":"abc"}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// The stub speaks plain HTTP, and registryBaseURL now honours an explicit http:// — so drive
	// the whole CheckRegistry path (probe → Bearer challenge → token) against srv.URL directly,
	// with nil roots (the system pool; TLS never enters into an http stub).
	if err := CheckRegistry(context.Background(), srv.URL, "good", "pw", nil); err != nil {
		t.Errorf("a valid credential was rejected: %v", err)
	}
	// A wrong credential must come back as ErrBadCredential — the create handler leans on that to
	// keep a bad password a hard error rather than an advisory "save anyway".
	if err := CheckRegistry(context.Background(), srv.URL, "bad", "nope", nil); !errors.Is(err, ErrBadCredential) {
		t.Errorf("a wrong credential should be ErrBadCredential, got %v", err)
	}
}
