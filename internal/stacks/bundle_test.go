package stacks

import (
	"archive/tar"
	"bytes"
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
