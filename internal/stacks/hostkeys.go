package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// DiscoverHostKeys returns the SSH host keys for a git host, as known_hosts lines ready to paste
// into a credential's pinned-keys field.
//
// This is the trustworthy half of "add a deploy key without running ssh-keyscan by hand". For
// github.com it does BETTER than ssh-keyscan: GitHub publishes its keys at api.github.com/meta
// over authenticated TLS, so the keys are verified, not trust-on-first-use. For every other host
// it performs the same handshake ssh-keyscan does — which is TOFU, no better and no worse than the
// command a person would otherwise run, but pre-filled and editable so at least it gets done.
//
// The caller is admin-gated (git credentials are a global setting), which is what bounds the
// obvious "make the server connect to an arbitrary host" concern: revealing a public SSH host key
// is not sensitive, and the port is fixed at 22.
func DiscoverHostKeys(ctx context.Context, host string) (lines []string, verified bool, err error) {
	host, err = normalizeHost(host)
	if err != nil {
		return nil, false, err
	}

	if host == "github.com" || host == "www.github.com" {
		lines, err := githubHostKeys(ctx)
		return lines, true, err // verified: fetched over authenticated TLS, not TOFU
	}

	lines, err = scanHostKeys(ctx, host)
	return lines, false, err
}

// normalizeHost pulls a bare hostname out of whatever a person pasted — a clone URL, a
// git@host:path SSH remote, or a host:22 — because those are what sits on the clipboard when you
// are setting up a deploy, and failing on them just to make the user re-type is hostile.
func normalizeHost(host string) (string, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return "", errors.New("a host is required")
	}
	host = strings.TrimPrefix(host, "ssh://")
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.LastIndex(host, "@"); i >= 0 { // git@github.com
		host = host[i+1:]
	}
	// Drop a path (github.com/acme/repo or github.com:acme/repo) and a :22 suffix.
	host = strings.SplitN(host, "/", 2)[0]
	host = strings.TrimSuffix(host, ":22")
	if i := strings.IndexByte(host, ':'); i >= 0 { // github.com:acme  → github.com
		host = host[:i]
	}
	if host == "" || strings.ContainsAny(host, " \t") {
		return "", fmt.Errorf("%q is not a host name", host)
	}
	return host, nil
}

// githubHostKeys reads GitHub's published keys. The /meta document is served over TLS from a host
// whose certificate we verify, so unlike a keyscan these keys are authenticated.
func githubHostKeys(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/meta", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching GitHub's published keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub's metadata endpoint returned %s", resp.Status)
	}

	var meta struct {
		SSHKeys []string `json:"ssh_keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decoding GitHub's metadata: %w", err)
	}
	if len(meta.SSHKeys) == 0 {
		return nil, errors.New("GitHub's metadata carried no SSH keys")
	}

	out := make([]string, 0, len(meta.SSHKeys))
	for _, k := range meta.SSHKeys {
		out = append(out, "github.com "+strings.TrimSpace(k))
	}
	sort.Strings(out)
	return out, nil
}

// The algorithms worth asking for, in the order a modern server prefers them. The CLIENT picks
// which to negotiate, so a credential must pin EVERY type the server offers — see hostKeyCallback.
// We therefore handshake once per algorithm and collect each key it presents.
var scanAlgos = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA256,
	ssh.KeyAlgoRSA,
}

// errCaptured is returned from the host-key callback to abort the handshake the instant the key is
// in hand. There is no account to log into and no wish to try — the key is presented before
// authentication, which is the whole reason a keyscan needs no credentials.
var errCaptured = errors.New("host key captured")

func scanHostKeys(ctx context.Context, host string) ([]string, error) {
	seen := map[string]bool{}
	var lines []string

	for _, algo := range scanAlgos {
		key, err := scanOne(ctx, host, algo)
		if err != nil {
			continue // this server does not offer this type; try the next
		}
		line := host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}

	if len(lines) == 0 {
		return nil, fmt.Errorf("no SSH host key could be read from %s — is it reachable on port 22?", host)
	}
	sort.Strings(lines)
	return lines, nil
}

func scanOne(ctx context.Context, host, algo string) (ssh.PublicKey, error) {
	var captured ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User:              "daffa-keyscan",
		HostKeyAlgorithms: []string{algo},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			return errCaptured
		},
		Timeout: 8 * time.Second,
	}

	d := net.Dialer{Timeout: 8 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "22"))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	// The handshake presents the host key, our callback grabs it and aborts with errCaptured. Any
	// other error is a real failure (unreachable, or the algorithm was not offered).
	_, _, _, err = ssh.NewClientConn(conn, host, cfg)
	if captured != nil {
		return captured, nil
	}
	return nil, err
}
