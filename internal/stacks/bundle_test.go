package stacks

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"
)

// bundleFiles reads a bundle tar into a name→body map for assertions.
func bundleFiles(t *testing.T, b *Bundle) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b.Tar))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading bundle tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = string(body)
	}
	return out
}

func TestBundleWritesSecretsAsFiles(t *testing.T) {
	yaml := "services:\n  web:\n    image: nginx\n"
	secs := []Secret{
		{Name: "tls_key", Content: "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n"},
		{Name: "db_password", Content: "hunter2"},
	}

	b, err := Build(yaml, nil, nil) // no secrets: nothing under daffa-secrets/
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bundleFiles(t, b)["daffa-secrets/db_password"]; ok {
		t.Fatal("a bundle with no secrets must not create daffa-secrets/ files")
	}

	withSecs, err := BuildPlanned(yaml, &HookPlan{DeployYAML: yaml}, nil, secs, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := bundleFiles(t, withSecs)
	if got := files["daffa-secrets/db_password"]; got != "hunter2" {
		t.Errorf("db_password = %q, want hunter2", got)
	}
	if got := files["daffa-secrets/tls_key"]; got != secs[0].Content {
		t.Errorf("tls_key body not preserved: %q", got)
	}
}

// A rotated secret is a changed deployment: the hash must move, or drift detection would
// report a stack as unchanged after its key was rotated.
func TestBundleHashCoversSecretContent(t *testing.T) {
	yaml := "services:\n  web:\n    image: nginx\n"

	a, err := BuildPlanned(yaml, &HookPlan{DeployYAML: yaml}, nil, []Secret{{Name: "k", Content: "v1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildPlanned(yaml, &HookPlan{DeployYAML: yaml}, nil, []Secret{{Name: "k", Content: "v2"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash == b.Hash {
		t.Fatal("rotating a secret must change the bundle hash")
	}

	// Order-independent: the same set in a different order is the same deployment.
	x, _ := BuildPlanned(yaml, &HookPlan{DeployYAML: yaml}, nil, []Secret{{Name: "a", Content: "1"}, {Name: "b", Content: "2"}}, nil)
	y, _ := BuildPlanned(yaml, &HookPlan{DeployYAML: yaml}, nil, []Secret{{Name: "b", Content: "2"}, {Name: "a", Content: "1"}}, nil)
	if x.Hash != y.Hash {
		t.Fatal("secret ordering must not affect the hash")
	}
}

func TestBundleRefusesUnsafeSecretName(t *testing.T) {
	yaml := "services: {}\n"
	for _, bad := range []string{"../escape", "a/b", ""} {
		_, err := BuildPlanned(yaml, &HookPlan{DeployYAML: yaml}, nil, []Secret{{Name: bad, Content: "x"}}, nil)
		if err == nil {
			t.Errorf("secret name %q must be refused", bad)
		}
	}
}

// dockerConfig has to encode two credential shapes differently: a normal user+password is HTTP
// Basic, but a bare token (no username) has to be a `registrytoken` — base64(":token") is a
// malformed Basic header that registries reject, which was the original bug.
func TestDockerConfigBasicVsToken(t *testing.T) {
	cfg, err := dockerConfig([]*RegistryAuth{
		{URL: "registry.example.com", Username: "deploy", Password: "s3cret"},
		{URL: "ghcr.io", Username: "", Password: "ghp_token"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			RegistryToken string `json:"registrytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(cfg), &got); err != nil {
		t.Fatalf("config.json is not valid JSON: %v\n%s", err, cfg)
	}
	if len(got.Auths) != 2 {
		t.Fatalf("want one entry per registry, got %d: %s", len(got.Auths), cfg)
	}

	// Username present → basic auth, and NO registrytoken.
	basic := got.Auths["registry.example.com"]
	if want := base64.StdEncoding.EncodeToString([]byte("deploy:s3cret")); basic.Auth != want {
		t.Errorf("basic auth = %q, want %q", basic.Auth, want)
	}
	if basic.RegistryToken != "" {
		t.Errorf("a credential with a username must not carry a registrytoken: %q", basic.RegistryToken)
	}

	// Username empty → registrytoken (bearer), and NO basic auth (not base64(":token")).
	tok := got.Auths["ghcr.io"]
	if tok.RegistryToken != "ghp_token" {
		t.Errorf("registrytoken = %q, want ghp_token", tok.RegistryToken)
	}
	if tok.Auth != "" {
		t.Errorf("a username-less credential must not emit a basic auth (was %q — the base64(\":token\") bug)", tok.Auth)
	}
}

// No credentials means no config.json in the bundle at all — an anonymous pull needs no auth file.
func TestBundleOmitsConfigWhenNoAuths(t *testing.T) {
	yaml := "services:\n  web:\n    image: nginx\n"
	for _, auths := range [][]*RegistryAuth{nil, {}} {
		b, err := Build(yaml, nil, auths)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := bundleFiles(t, b)["config.json"]; ok {
			t.Errorf("a bundle with %d auths must not write config.json", len(auths))
		}
	}
}
