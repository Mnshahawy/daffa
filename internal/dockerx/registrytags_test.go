package dockerx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseImageRef(t *testing.T) {
	cases := []struct {
		in                      string
		host, repo, tag, digest string
	}{
		{"postgres:16", "docker.io", "library/postgres", "16", ""},
		{"nginx", "docker.io", "library/nginx", "latest", ""},
		{"ghcr.io/acme/api:1.2.3", "ghcr.io", "acme/api", "1.2.3", ""},
		{"quay.io/prometheus/prometheus:v2.53.0", "quay.io", "prometheus/prometheus", "v2.53.0", ""},
		{"redis@sha256:" + strings.Repeat("a", 64), "docker.io", "library/redis", "", "sha256:" + strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		got, err := ParseImageRef(c.in)
		if err != nil {
			t.Errorf("ParseImageRef(%q): %v", c.in, err)
			continue
		}
		if got.Host != c.host || got.Repo != c.repo || got.Tag != c.tag || got.Digest != c.digest {
			t.Errorf("ParseImageRef(%q) = %+v; want host=%s repo=%s tag=%s digest=%s",
				c.in, got, c.host, c.repo, c.tag, c.digest)
		}
	}

	// An unresolved interpolation is not a ref; callers rely on the error to classify it.
	if _, err := ParseImageRef("app:"); err == nil {
		t.Error("an empty tag should not parse as a ref")
	}
}

func TestRegistryHostNormalises(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io":                   "docker.io",
		"https://index.docker.io/v1/": "docker.io",
		"registry-1.docker.io":        "docker.io",
		"ghcr.io":                     "ghcr.io",
		"https://quay.io/":            "quay.io",
	} {
		if got := RegistryHost(in); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// The manifest read is the whole of tag validation: a 401 challenge must be followed to the
// token endpoint WITH the repo pull scope, and only then does the manifest come back. The stub
// withholds the token unless the scope was forwarded, so a dropped scope shows up as "not found".
func TestManifestGetFollowsScopedBearerChallenge(t *testing.T) {
	const repo = "acme/api"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/"+repo+"/manifests/"):
			if r.Header.Get("Authorization") != "Bearer abc" {
				w.Header().Set("Www-Authenticate",
					`Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:`+repo+`:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/1.0") {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case r.URL.Path == "/token":
			// Only mint the token if the pull scope was forwarded from the challenge.
			if r.URL.Query().Get("scope") == "repository:"+repo+":pull" {
				_, _ = w.Write([]byte(`{"token":"abc"}`))
			} else {
				w.WriteHeader(http.StatusUnauthorized)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := srv.Client()
	// A tag that exists → 200 (proves the scoped token was obtained and reused).
	resp, err := registryAuthedGet(context.Background(), client, srv.URL+"/v2/"+repo+"/manifests/1.0", manifestAccept, "", "")
	if err != nil {
		t.Fatalf("existing tag: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("existing tag answered %d; want 200 (scope not forwarded?)", resp.StatusCode)
	}
	// A tag that does not exist → 404, cleanly.
	resp, err = registryAuthedGet(context.Background(), client, srv.URL+"/v2/"+repo+"/manifests/9.9", manifestAccept, "", "")
	if err != nil {
		t.Fatalf("missing tag: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing tag answered %d; want 404", resp.StatusCode)
	}
}
