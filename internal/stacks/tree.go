package stacks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The ceilings exist because this is config delivery, not artifact distribution: config is
// measured in kilobytes, a plugin directory in megabytes, and anything past that should be
// a registry image. Vars, not consts, so tests can lower them without committing 32 MiB of
// fixture. The refusal messages say what the right vehicle is.
var (
	maxTreeFiles     = 4096
	maxTreeFileBytes = int64(8 << 20)
	maxTreeBytes     = int64(32 << 20)
)

// ResolvedTree is a git subtree: every file under the source's Path at the resolved
// commit, ready to become the contents of a named volume.
type ResolvedTree struct {
	Files         []TreeFile
	CommitSHA     string
	CommitSubject string
	// Warnings are true statements about content that synced anyway — say-so, then defer
	// to the operator. (A private key in a repo is their emergency, not our refusal.)
	Warnings []string
}

// TreeFile is one file of a resolved subtree. Name is relative to the subtree, always
// clean, never absolute. Mode is 0755 when git says executable, 0644 otherwise — nothing
// else survives resolution, so a repo cannot smuggle a setuid bit into a volume.
type TreeFile struct {
	Name string
	Data []byte
	Mode int64
}

// ResolveTree fetches every file under src.Path at the source's ref. Same shallow,
// single-branch, in-memory clone as Resolve — nothing touches the server's disk — but it
// walks a directory instead of reading one blob.
func ResolveTree(ctx context.Context, src Source) (*ResolvedTree, error) {
	if src.Kind != "git" {
		return nil, fmt.Errorf("stacks: a volume source needs a git repository; %q has no tree to resolve", src.Kind)
	}
	c, err := cloneCommit(ctx, src)
	if err != nil {
		return nil, err
	}
	files, warnings, err := treeFiles(c, src.Path)
	if err != nil {
		return nil, err
	}
	return &ResolvedTree{
		Files:         files,
		CommitSHA:     c.Hash.String(),
		CommitSubject: subjectOf(c.Message),
		Warnings:      warnings,
	}, nil
}

// treeFiles walks the subtree at dir and returns its regular files, refusing anything
// that is not one. Split from ResolveTree so tests can exercise every refusal against an
// in-memory commit without a remote to clone.
func treeFiles(c *object.Commit, dir string) ([]TreeFile, []string, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("stacks: reading tree: %w", err)
	}

	dir = path.Clean(strings.TrimPrefix(dir, "/"))
	sub := tree
	if dir != "" && dir != "." {
		sub, err = tree.Tree(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("stacks: the repository has no directory %s at this ref", dir)
		}
	}

	w := object.NewTreeWalker(sub, true, nil)
	defer w.Close()

	var files []TreeFile
	var warnings []string
	var total int64
	for {
		name, entry, err := w.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("stacks: walking %s: %w", describeDir(dir), err)
		}

		switch entry.Mode {
		case filemode.Dir:
			continue
		case filemode.Symlink:
			// A symlink extracted into a volume dereferences inside the CONSUMING
			// container: `config.yml -> /run/secrets/db_password` turns a config volume
			// into an exfiltration tool. Refused, not skipped — a silent skip would
			// deliver a subtree that is not the one in the repo.
			return nil, nil, fmt.Errorf("stacks: %s is a symlink, which a volume cannot carry safely — vendor the target file instead", path.Join(dir, name))
		case filemode.Submodule:
			return nil, nil, fmt.Errorf("stacks: %s is a git submodule, which a shallow clone does not carry — vendor the files, or point a separate volume source at that repository", path.Join(dir, name))
		case filemode.Regular, filemode.Executable, filemode.Deprecated:
			// Deprecated is git's old group-writable regular mode; content-wise a file.
		default:
			return nil, nil, fmt.Errorf("stacks: %s has tree mode %s, which has no business in a volume", path.Join(dir, name), entry.Mode)
		}

		// Git itself refuses to store "..", absolute paths, and empty segments, so this
		// should be unreachable — but "should be unreachable" is not a property to
		// extract a tar archive on.
		if name != path.Clean(name) || strings.HasPrefix(name, "/") ||
			name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
			return nil, nil, fmt.Errorf("stacks: %q does not stay inside the subtree", name)
		}

		if len(files) >= maxTreeFiles {
			return nil, nil, fmt.Errorf("stacks: %s holds more than %d files — this is config delivery, not artifact distribution; anything that big belongs in a registry image", describeDir(dir), maxTreeFiles)
		}

		f, err := sub.TreeEntryFile(&object.TreeEntry{Name: name, Mode: entry.Mode, Hash: entry.Hash})
		if err != nil {
			return nil, nil, fmt.Errorf("stacks: reading %s: %w", path.Join(dir, name), err)
		}
		if f.Size > maxTreeFileBytes {
			return nil, nil, fmt.Errorf("stacks: %s is %d bytes; the per-file ceiling is %d — a file that size is an artifact, and the vehicle for artifacts is a registry image", path.Join(dir, name), f.Size, maxTreeFileBytes)
		}
		total += f.Size
		if total > maxTreeBytes {
			return nil, nil, fmt.Errorf("stacks: %s exceeds %d bytes in total — a subtree that size is an artifact, and the vehicle for artifacts is a registry image", describeDir(dir), maxTreeBytes)
		}

		rc, err := f.Blob.Reader()
		if err != nil {
			return nil, nil, fmt.Errorf("stacks: reading %s: %w", path.Join(dir, name), err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("stacks: reading %s: %w", path.Join(dir, name), err)
		}

		mode := int64(0o644)
		if entry.Mode == filemode.Executable {
			mode = 0o755
		}
		if looksLikePrivateKey(b) {
			warnings = append(warnings, fmt.Sprintf("%s looks like it carries a private key — a repository is the wrong place for one; secrets belong in sealed stack env vars or a cert delivery", path.Join(dir, name)))
		}
		files = append(files, TreeFile{Name: name, Data: b, Mode: mode})
	}

	if len(files) == 0 {
		// Git cannot even represent an empty directory, so this means the path pointed
		// at nothing deliverable. An empty sync that then mirror-deletes a volume's
		// contents is not a state to reach by accident.
		return nil, nil, fmt.Errorf("stacks: %s contains no files at this ref", describeDir(dir))
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, warnings, nil
}

func describeDir(dir string) string {
	if dir == "" || dir == "." {
		return "the repository root"
	}
	return dir
}

// looksLikePrivateKey flags the two shapes of key material this codebase already knows by
// name: age identities and PEM private keys. A match warns, never blocks — the operator
// may genuinely want a test fixture delivered, but a real key in a repo is a problem
// Daffa should say out loud.
func looksLikePrivateKey(b []byte) bool {
	return bytes.Contains(b, []byte("AGE-SECRET-KEY-")) ||
		bytes.Contains(b, []byte("PRIVATE KEY-----"))
}
