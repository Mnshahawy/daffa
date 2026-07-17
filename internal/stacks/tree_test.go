package stacks

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

type fixtureFile struct {
	data    string
	exec    bool
	symlink string // when set, the entry is a symlink to this target
}

// commitFixture builds a one-commit in-memory repository, so every refusal in treeFiles
// can be exercised without a remote to clone.
func commitFixture(t *testing.T, files map[string]fixtureFile) *object.Commit {
	t.Helper()

	fs := memfs.New()
	repo, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	for name, f := range files {
		if f.symlink != "" {
			if err := fs.Symlink(f.symlink, name); err != nil {
				t.Fatalf("symlink %s: %v", name, err)
			}
		} else {
			mode := fsModeFor(f.exec)
			if err := util.WriteFile(fs, name, []byte(f.data), mode); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}

	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	hash, err := wt.Commit("fixture commit", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	c, err := repo.CommitObject(hash)
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	return c
}

func fsModeFor(exec bool) os.FileMode {
	if exec {
		return 0o755
	}
	return 0o644
}

func TestTreeFilesSubtree(t *testing.T) {
	c := commitFixture(t, map[string]fixtureFile{
		"README.md":               {data: "not delivered"},
		"traefik/dynamic/tls.yml": {data: "tls: {}\n"},
		"traefik/dynamic/b.yml":   {data: "b: 1\n"},
		"traefik/dynamic/hook.sh": {data: "#!/bin/sh\n", exec: true},
	})

	files, warnings, err := treeFiles(c, "traefik/dynamic")
	if err != nil {
		t.Fatalf("treeFiles: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3 (README.md must not leak in): %v", len(files), names(files))
	}
	// Sorted, subtree-relative names.
	want := []string{"b.yml", "hook.sh", "tls.yml"}
	for i, n := range want {
		if files[i].Name != n {
			t.Fatalf("file %d is %s, want %s", i, files[i].Name, n)
		}
	}
	if files[1].Mode != 0o755 {
		t.Errorf("hook.sh mode %o, want 755 — git's executable bit must survive", files[1].Mode)
	}
	if files[0].Mode != 0o644 || files[2].Mode != 0o644 {
		t.Errorf("plain files must normalize to 0644")
	}
	if string(files[2].Data) != "tls: {}\n" {
		t.Errorf("tls.yml content: %q", files[2].Data)
	}
}

func TestTreeFilesRepositoryRoot(t *testing.T) {
	c := commitFixture(t, map[string]fixtureFile{
		"a.yml":     {data: "a"},
		"sub/b.yml": {data: "b"},
	})

	for _, dir := range []string{"", ".", "/"} {
		files, _, err := treeFiles(c, dir)
		if err != nil {
			t.Fatalf("treeFiles(%q): %v", dir, err)
		}
		if len(files) != 2 {
			t.Fatalf("treeFiles(%q): got %v, want a.yml and sub/b.yml", dir, names(files))
		}
		if files[1].Name != "sub/b.yml" {
			t.Fatalf("treeFiles(%q): nested name %s, want sub/b.yml", dir, files[1].Name)
		}
	}
}

func TestTreeFilesRefusesSymlink(t *testing.T) {
	c := commitFixture(t, map[string]fixtureFile{
		"cfg/ok.yml":     {data: "ok"},
		"cfg/config.yml": {symlink: "/run/secrets/db_password"},
	})

	_, _, err := treeFiles(c, "cfg")
	if err == nil {
		t.Fatal("a symlink must be refused, not skipped")
	}
	if !strings.Contains(err.Error(), "cfg/config.yml") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("the refusal must name the file and the reason: %v", err)
	}
}

func TestTreeFilesMissingDir(t *testing.T) {
	c := commitFixture(t, map[string]fixtureFile{"a.yml": {data: "a"}})

	_, _, err := treeFiles(c, "no/such/dir")
	if err == nil || !strings.Contains(err.Error(), "no/such/dir") {
		t.Fatalf("a missing directory must be named in the error, got: %v", err)
	}
}

func TestTreeFilesPerFileCeiling(t *testing.T) {
	old := maxTreeFileBytes
	maxTreeFileBytes = 8
	defer func() { maxTreeFileBytes = old }()

	c := commitFixture(t, map[string]fixtureFile{
		"cfg/big.bin": {data: "123456789"}, // 9 bytes > 8
	})

	_, _, err := treeFiles(c, "cfg")
	if err == nil || !strings.Contains(err.Error(), "cfg/big.bin") || !strings.Contains(err.Error(), "registry image") {
		t.Fatalf("an oversize file must be refused by name, pointing at the right vehicle: %v", err)
	}
}

func TestTreeFilesTotalCeiling(t *testing.T) {
	old := maxTreeBytes
	maxTreeBytes = 10
	defer func() { maxTreeBytes = old }()

	c := commitFixture(t, map[string]fixtureFile{
		"cfg/a.yml": {data: "12345678"},
		"cfg/b.yml": {data: "12345678"},
	})

	_, _, err := treeFiles(c, "cfg")
	if err == nil || !strings.Contains(err.Error(), "in total") {
		t.Fatalf("a subtree over the total ceiling must be refused: %v", err)
	}
}

func TestTreeFilesFileCountCeiling(t *testing.T) {
	old := maxTreeFiles
	maxTreeFiles = 2
	defer func() { maxTreeFiles = old }()

	c := commitFixture(t, map[string]fixtureFile{
		"cfg/a.yml": {data: "a"},
		"cfg/b.yml": {data: "b"},
		"cfg/c.yml": {data: "c"},
	})

	_, _, err := treeFiles(c, "cfg")
	if err == nil || !strings.Contains(err.Error(), "more than 2 files") {
		t.Fatalf("a subtree over the file-count ceiling must be refused: %v", err)
	}
}

func TestTreeFilesWarnsOnPrivateKeyMaterial(t *testing.T) {
	c := commitFixture(t, map[string]fixtureFile{
		"cfg/ok.yml":     {data: "fine"},
		"cfg/leaked":     {data: "AGE-SECRET-KEY-1QQQQQQQQ"},
		"cfg/leaked.pem": {data: "-----BEGIN EC PRIVATE KEY-----\nMHc...\n-----END EC PRIVATE KEY-----\n"},
	})

	files, warnings, err := treeFiles(c, "cfg")
	if err != nil {
		t.Fatalf("key material must warn, not block: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("all files must still deliver, got %v", names(files))
	}
	if len(warnings) != 2 {
		t.Fatalf("got %d warnings, want 2: %v", len(warnings), warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "private key") {
			t.Errorf("warning must say what it saw: %q", w)
		}
	}
}

func TestTreeFilesEmptySubtreeRefused(t *testing.T) {
	// Git cannot represent an empty directory, so "no files" means the source points at
	// nothing deliverable — and an empty sync must never become a mirror-wipe.
	c := commitFixture(t, map[string]fixtureFile{"a.yml": {data: "a"}})

	_, _, err := treeFiles(c, "a.yml/nothing")
	if err == nil {
		t.Fatal("expected an error for a path with no files under it")
	}
}

func names(files []TreeFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Name
	}
	return out
}
