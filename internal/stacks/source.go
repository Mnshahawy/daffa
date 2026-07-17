package stacks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Source describes where a stack's compose file comes from.
type Source struct {
	Kind string // git | inline
	URL  string
	Ref  string // branch, tag, or commit
	Path string // path to the compose file inside the repo
	YAML string // inline only

	// Auth is the resolved credential, or nil for a public repository.
	Auth *GitAuth
}

// GitAuth is a credential in plaintext, resolved just in time and never stored this way.
type GitAuth struct {
	Kind       string // token | ssh
	Username   string
	Token      string
	SSHKey     string // PEM private key
	Passphrase string
	HostKey    string // one line of ssh-keyscan output; empty = unpinned
}

// IsSSHURL reports whether a URL needs the SSH transport. Both forms are common:
// scp-style (git@host:org/repo.git) and a real URL (ssh://git@host/org/repo.git).
func IsSSHURL(url string) bool {
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") ||
		strings.HasPrefix(url, "git://") {
		return false
	}
	// scp-style: user@host:path — a colon before any slash.
	at := strings.Index(url, "@")
	colon := strings.Index(url, ":")
	slash := strings.Index(url, "/")
	return at > 0 && colon > at && (slash < 0 || colon < slash)
}

const gitTimeout = 60 * time.Second

// Resolved is a compose file, plus what it came from.
//
// The commit is not decoration. A bundle hash can tell you the source moved; only a commit can
// tell you WHICH commit is running, and "which commit is in production?" is the question an
// operator actually asks. The clone already had it in hand and used to throw it away.
type Resolved struct {
	YAML string
	// CommitSHA and CommitSubject are empty for an inline source, which has no commit and
	// should not pretend to.
	CommitSHA     string
	CommitSubject string
}

// Resolve fetches the compose file the source points at.
//
// The clone is shallow, in memory, and single-branch: we want one file at one ref, not a
// history. Nothing touches the server's disk, which matters because Daffa may be running
// from a read-only filesystem and because a repo left in /tmp is a repo someone else can
// read.
func Resolve(ctx context.Context, src Source) (*Resolved, error) {
	switch src.Kind {
	case "inline":
		if strings.TrimSpace(src.YAML) == "" {
			return nil, errors.New("stacks: the inline compose file is empty")
		}
		return &Resolved{YAML: src.YAML}, nil

	case "git":
		return resolveGit(ctx, src)

	default:
		return nil, fmt.Errorf("stacks: unknown source kind %q", src.Kind)
	}
}

// cloneCommit is the shared front half of resolving anything from git: shallow in-memory
// single-branch clone, then the commit the source's ref points at. Both the compose-file
// path (Resolve) and the subtree path (ResolveTree) start here.
func cloneCommit(ctx context.Context, src Source) (*object.Commit, error) {
	if src.URL == "" {
		return nil, errors.New("stacks: a git URL is required")
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	opts := &git.CloneOptions{
		URL:          src.URL,
		Depth:        1,
		SingleBranch: true,
		Tags:         git.NoTags,
	}
	if src.Ref != "" && !isCommitSHA(src.Ref) {
		// A branch or tag can be asked for by name at clone time. A commit SHA cannot
		// (git refuses to clone a bare SHA), so those are handled below.
		opts.ReferenceName = refName(src.Ref)
	}

	auth, err := transportAuth(src)
	if err != nil {
		return nil, err
	}
	opts.Auth = auth

	repo, err := git.CloneContext(ctx, memory.NewStorage(), memfs.New(), opts)
	if err != nil {
		return nil, fmt.Errorf("stacks: cloning %s: %w", src.URL, friendlyGitError(err))
	}

	ref, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("stacks: reading HEAD: %w", err)
	}
	commit := ref.Hash()

	if src.Ref != "" && isCommitSHA(src.Ref) {
		// A shallow clone of the default branch will not contain an arbitrary commit,
		// so say so rather than silently deploying the wrong thing.
		commit = plumbing.NewHash(src.Ref)
		if _, err := repo.CommitObject(commit); err != nil {
			return nil, fmt.Errorf(
				"stacks: commit %s is not reachable from a shallow clone — use a branch or tag, "+
					"or a commit on the default branch's tip", src.Ref)
		}
	}

	c, err := repo.CommitObject(commit)
	if err != nil {
		return nil, fmt.Errorf("stacks: reading commit: %w", err)
	}
	return c, nil
}

func resolveGit(ctx context.Context, src Source) (*Resolved, error) {
	c, err := cloneCommit(ctx, src)
	if err != nil {
		return nil, err
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("stacks: reading tree: %w", err)
	}

	file := src.Path
	if file == "" {
		file = "docker-compose.yml"
	}
	file = path.Clean(strings.TrimPrefix(file, "/"))

	f, err := tree.File(file)
	if err != nil {
		return nil, fmt.Errorf("stacks: %s does not contain %s at %s", src.URL, file, describeRef(src.Ref))
	}

	rc, err := f.Blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("stacks: reading %s: %w", file, err)
	}
	defer rc.Close()

	b, err := io.ReadAll(io.LimitReader(rc, 1<<20)) // a compose file is not a megabyte
	if err != nil {
		return nil, fmt.Errorf("stacks: reading %s: %w", file, err)
	}

	return &Resolved{
		YAML:      string(b),
		CommitSHA: c.Hash.String(),
		// The subject only — the first line. A commit body can be paragraphs, and this is
		// going in a table cell.
		CommitSubject: subjectOf(c.Message),
	}, nil
}

// subjectOf takes the first line of a commit message and keeps it to a length that fits where
// it is going to be shown.
func subjectOf(msg string) string {
	subject := strings.TrimSpace(msg)
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = strings.TrimSpace(subject[:i])
	}
	const max = 200
	if len(subject) > max {
		subject = subject[:max] + "…"
	}
	return subject
}

// refName guesses whether a ref is a branch or a tag. go-git needs to be told which, and
// getting it wrong is a confusing "reference not found" — so try branch first (the
// overwhelmingly common case) and let the caller's tag land through the tag namespace.
func refName(ref string) plumbing.ReferenceName {
	switch {
	case strings.HasPrefix(ref, "refs/"):
		return plumbing.ReferenceName(ref)
	case strings.HasPrefix(ref, "v") || strings.Contains(ref, "."):
		// Looks like a version tag. If it is actually a branch, the clone fails with a
		// clear message and the user can write refs/heads/<name> explicitly.
		return plumbing.NewTagReferenceName(ref)
	default:
		return plumbing.NewBranchReferenceName(ref)
	}
}

func isCommitSHA(ref string) bool {
	if len(ref) != 40 && len(ref) != 7 {
		return false
	}
	for _, r := range ref {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func describeRef(ref string) string {
	if ref == "" {
		return "the default branch"
	}
	return ref
}

// friendlyGitError turns go-git's terse errors into something an operator can act on.
func friendlyGitError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "authentication required"),
		strings.Contains(msg, "authorization failed"):
		return errors.New("authentication failed — a private repository needs an access token")
	case strings.Contains(msg, "reference not found"):
		return errors.New("that branch or tag does not exist")
	case strings.Contains(msg, "repository not found"):
		return errors.New("repository not found (or the credential cannot see it)")
	// Order matters here. go-git reports a host-key mismatch as "ssh: handshake failed:
	// knownhosts: key mismatch", so a check for "handshake failed" would swallow it and
	// blame the deploy key — sending someone off to re-add a key that was never the
	// problem. Look for the specific cause first.
	case strings.Contains(msg, "knownhosts"),
		strings.Contains(msg, "key mismatch"),
		strings.Contains(msg, "host key"):
		return errors.New("the server's SSH host key does not match the one pinned on this credential — " +
			"if the server was legitimately rebuilt or rotated, update the credential with fresh `ssh-keyscan` output")
	case strings.Contains(msg, "unable to authenticate"),
		strings.Contains(msg, "handshake failed"):
		return errors.New("the git server rejected the SSH key — is its public half added to the repository as a deploy key?")
	default:
		return err
	}
}
