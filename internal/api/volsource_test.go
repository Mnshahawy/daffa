package api

import (
	"testing"

	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

func TestTouchesSubtree(t *testing.T) {
	cases := []struct {
		files   []string
		subtree string
		want    bool
	}{
		{[]string{"traefik/dynamic/tls.yml"}, "traefik/dynamic", true},
		{[]string{"traefik/dynamic"}, "traefik/dynamic", true},
		{[]string{"README.md", "app/main.go"}, "traefik/dynamic", false},
		// A sibling with the subtree as a name PREFIX is not inside it.
		{[]string{"traefik/dynamic-old/tls.yml"}, "traefik/dynamic", false},
		// Root subtree: everything is inside.
		{[]string{"anything"}, "", true},
		{[]string{"anything"}, "/", true},
		{[]string{"anything"}, ".", true},
		// Leading slashes on pushed paths do not dodge the match.
		{[]string{"/traefik/dynamic/a.yml"}, "traefik/dynamic", true},
	}
	for _, c := range cases {
		if got := touchesSubtree(c.files, c.subtree); got != c.want {
			t.Errorf("touchesSubtree(%v, %q) = %v, want %v", c.files, c.subtree, got, c.want)
		}
	}
}

func TestVolumeSourceHashIsContentOnly(t *testing.T) {
	tree := func(commit string) *stacks.ResolvedTree {
		return &stacks.ResolvedTree{
			CommitSHA: commit,
			Files: []stacks.TreeFile{
				{Name: "a.yml", Data: []byte("a: 1"), Mode: 0o644},
				{Name: "b.sh", Data: []byte("#!/bin/sh"), Mode: 0o755},
			},
		}
	}

	// A force-push that lands identical content must hash identically — otherwise every
	// restart target gets bounced for nothing.
	if volumeSourceHash(tree("commit-1"), 0, 0) != volumeSourceHash(tree("commit-2"), 0, 0) {
		t.Error("the hash must not depend on the commit")
	}

	// Ownership and mode are part of the desired state: flipping either must re-sync.
	if volumeSourceHash(tree("c"), 0, 0) == volumeSourceHash(tree("c"), 100, 100) {
		t.Error("the hash must change with uid/gid")
	}
	flipped := tree("c")
	flipped.Files[1].Mode = 0o644
	if volumeSourceHash(tree("c"), 0, 0) == volumeSourceHash(flipped, 0, 0) {
		t.Error("the hash must change with a file mode")
	}
}

// An inline volume source stores its files, round-trips its kind, and resolves to the same
// content tree a git source would — so the whole downstream sync (hash, manifest, write) is
// shared. No Docker here: the volume write itself is exercised by the verify harness.
func TestInlineVolumeSourceResolves(t *testing.T) {
	s, ctx := certServer(t)
	env, _, err := s.store.UpsertLocalEnvironment(ctx, "Local", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}

	v := &store.VolumeSource{EnvID: env.ID, Volume: "daffa-traefik-config", SourceKind: "inline"}
	if err := s.store.CreateVolumeSource(ctx, v); err != nil {
		t.Fatal(err)
	}
	files := []store.VolSourceFile{
		{Path: "traefik.yml", Content: "entryPoints: {}\n"},
		{Path: "dynamic/mw.yml", Content: "http: {}\n"},
	}
	if err := s.store.SetVolSourceFiles(ctx, v.ID, files); err != nil {
		t.Fatal(err)
	}

	got, err := s.store.VolumeSourceByID(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceKind != "inline" {
		t.Fatalf("SourceKind did not round-trip: %q", got.SourceKind)
	}

	rt, err := s.inlineTree(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Files) != 2 || rt.CommitSHA != "" {
		t.Fatalf("inlineTree: %d files, commit %q", len(rt.Files), rt.CommitSHA)
	}
	// The hash is deterministic and content-derived — the property the sync's skip-if-unchanged
	// relies on.
	if h1, h2 := volumeSourceHash(rt, got.UID, got.GID), volumeSourceHash(rt, got.UID, got.GID); h1 != h2 || h1 == "" {
		t.Fatalf("hash not stable: %q vs %q", h1, h2)
	}
}

// Switching an inline source to git must PERSIST the new kind and its git target — the store
// UPDATE omitted source_kind once, which let the handler flip it in memory while the row stayed
// inline, so the next scheduled sync re-read "inline" and delivered stale files. This locks the
// round-trip the handler's inline→git switch depends on, and that the dead inline files are cleared.
func TestVolumeSourceSwitchInlineToGitPersists(t *testing.T) {
	s, ctx := certServer(t)
	env, _, err := s.store.UpsertLocalEnvironment(ctx, "Local", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}

	v := &store.VolumeSource{EnvID: env.ID, Volume: "daffa-traefik-config", SourceKind: "inline"}
	if err := s.store.CreateVolumeSource(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetVolSourceFiles(ctx, v.ID, []store.VolSourceFile{{Path: "traefik.yml", Content: "x\n"}}); err != nil {
		t.Fatal(err)
	}

	// What the handler does after validating the inline→git switch and pre-flighting the repo.
	v.SourceKind, v.GitURL, v.GitRef = "git", "https://git.example.com/team/infra.git", "main"
	if err := s.store.UpdateVolumeSource(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetVolSourceFiles(ctx, v.ID, nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.store.VolumeSourceByID(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceKind != "git" || got.GitURL == "" {
		t.Fatalf("switch did not persist: kind=%q url=%q", got.SourceKind, got.GitURL)
	}
	files, err := s.store.VolSourceFiles(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("inline files not cleared after switch: %d remain", len(files))
	}
}

func TestVolSourceFilePathValidation(t *testing.T) {
	bad := [][]volSourceFileInput{
		{{Path: "/etc/passwd"}},
		{{Path: "../escape"}},
		{{Path: ""}},
		{{Path: "a"}, {Path: "a"}}, // duplicate
	}
	for _, in := range bad {
		if _, err := volSourceFilesFromRequest(in); err == nil {
			t.Errorf("expected rejection for %+v", in)
		}
	}
	if _, err := volSourceFilesFromRequest([]volSourceFileInput{{Path: "dynamic/mw.yml", Content: "x"}}); err != nil {
		t.Errorf("clean path rejected: %v", err)
	}
}
