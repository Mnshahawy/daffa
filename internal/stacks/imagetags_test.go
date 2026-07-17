package stacks

import (
	"strings"
	"testing"
)

func TestRewriteImageTags(t *testing.T) {
	in := `# my stack
services:
  web:
    image: nginx:1.25 # the frontend
    ports:
      - "80:80"
  cache:
    image: nginx:1.25
  db:
    image: postgres:16
`
	out, err := RewriteImageTags(in, map[string]string{
		"nginx:1.25":  "nginx:1.27",
		"postgres:16": "postgres:17",
	})
	if err != nil {
		t.Fatalf("RewriteImageTags: %v", err)
	}

	// Every service sharing an image is updated, and no stale value survives.
	if n := strings.Count(out, "nginx:1.27"); n != 2 {
		t.Errorf("expected both nginx services bumped, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "nginx:1.25") {
		t.Errorf("a stale nginx tag remains:\n%s", out)
	}
	if !strings.Contains(out, "postgres:17") {
		t.Errorf("postgres was not bumped:\n%s", out)
	}
	// The whole point of editing the tree: comments and layout survive.
	if !strings.Contains(out, "# the frontend") || !strings.Contains(out, "# my stack") {
		t.Errorf("comments were lost:\n%s", out)
	}
}

func TestRewriteImageTagsExactMatchOnly(t *testing.T) {
	in := "services:\n  a:\n    image: postgres:16\n  b:\n    image: postgres:16.2\n"
	out, err := RewriteImageTags(in, map[string]string{"postgres:16": "postgres:17"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "image: postgres:17") {
		t.Fatalf("postgres:16 was not bumped:\n%s", out)
	}
	// The prefix trap: 16.2 must not be touched when 16 is the target.
	if !strings.Contains(out, "postgres:16.2") {
		t.Errorf("postgres:16.2 was wrongly rewritten:\n%s", out)
	}
}
