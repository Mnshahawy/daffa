package stacks

import (
	"bytes"
	"fmt"
	"net"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// transportAuth turns a stored credential into whatever the transport wants, and refuses
// the combinations that cannot work — an SSH key will not authenticate an https:// clone,
// and a token means nothing to sshd. Catching that here produces a sentence someone can
// act on, instead of a transport-level error thirty seconds into a deploy.
func transportAuth(src Source) (transport.AuthMethod, error) {
	isSSH := IsSSHURL(src.URL)

	if src.Auth == nil {
		if isSSH {
			return nil, fmt.Errorf(
				"this is an SSH URL, so it needs an SSH credential — add one under Settings → Git, or use the repository's https:// URL")
		}
		return nil, nil // a public repository over https
	}

	switch src.Auth.Kind {
	case "ssh":
		if !isSSH {
			return nil, fmt.Errorf(
				"that credential is an SSH key, but %s is not an SSH URL — use the repository's SSH URL (git@host:org/repo.git) or pick a token credential",
				src.URL)
		}
		return sshAuth(src.Auth)

	case "token":
		if isSSH {
			return nil, fmt.Errorf(
				"that credential is an access token, but %s is an SSH URL — tokens work over https:// only",
				src.URL)
		}
		// Forgejo, GitHub and GitLab all accept a token as the password with any
		// non-empty username.
		user := src.Auth.Username
		if user == "" {
			user = "daffa"
		}
		return &githttp.BasicAuth{Username: user, Password: src.Auth.Token}, nil

	default:
		return nil, fmt.Errorf("stacks: unknown credential kind %q", src.Auth.Kind)
	}
}

// CheckSSHKey validates a key (and its passphrase, and any pinned host key) at the moment
// someone pastes it in, rather than at the moment a deploy needs it. A wrong passphrase
// discovered now is a typo; discovered during a deploy it is an outage with a confusing
// error attached.
func CheckSSHKey(key, passphrase, hostKey string) error {
	if _, err := sshAuth(&GitAuth{SSHKey: key, Passphrase: passphrase, HostKey: hostKey}); err != nil {
		return err
	}
	return nil
}

func sshAuth(a *GitAuth) (transport.AuthMethod, error) {
	// The user is part of the URL for scp-style remotes (git@host:…), and go-git takes it
	// from there; the one we pass is only a fallback.
	user := a.Username
	if user == "" {
		user = "git"
	}

	keys, err := gitssh.NewPublicKeys(user, []byte(a.SSHKey), a.Passphrase)
	if err != nil {
		if strings.Contains(err.Error(), "passphrase") || strings.Contains(err.Error(), "decrypt") {
			return nil, fmt.Errorf("the SSH key is encrypted and the passphrase is missing or wrong")
		}
		return nil, fmt.Errorf("that SSH private key could not be read: %w", err)
	}

	cb, err := hostKeyCallback(a.HostKey)
	if err != nil {
		return nil, err
	}
	keys.HostKeyCallback = cb

	return keys, nil
}

// hostKeyCallback decides whether we trust the server on the other end.
//
// With a pinned host key we verify it, which is what you want: a git server that has been
// substituted underneath you would otherwise hand a deploy someone else's compose file,
// and Daffa would run it.
//
// Without one we accept whatever answers. That is a real weakening and it is offered
// anyway, because the alternative is that nobody can clone from their internal Forgejo
// until they have worked out ssh-keyscan — and a tool that is impossible to start with
// gets replaced by a shell script that has no host checking at all. The UI says plainly
// when a credential is unpinned.
// It accepts MULTIPLE lines, and it has to. A server has several host keys (ed25519,
// ecdsa, rsa) and the CLIENT picks which algorithm to negotiate — Go's ssh package
// prefers ecdsa. So a credential pinned to only the ed25519 line would fail against a
// server that is perfectly honest, because the key it presented was a different one of
// its own. A known_hosts file holds every type for exactly this reason, and so does this:
// paste the whole output of `ssh-keyscan <host>` and any of its keys is accepted.
func hostKeyCallback(hostKey string) (ssh.HostKeyCallback, error) {
	hostKey = strings.TrimSpace(hostKey)
	if hostKey == "" {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // documented, and surfaced in the UI
	}

	var pinned []ssh.PublicKey
	for _, line := range strings.Split(hostKey, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Accept a full known_hosts line ("host ssh-ed25519 AAAA…") or a bare key.
		fields := strings.Fields(line)
		if len(fields) >= 3 && !strings.Contains(fields[0], "ssh-") && !strings.HasPrefix(fields[0], "ecdsa-") {
			fields = fields[1:] // drop the host column
		}

		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields, " ")))
		if err != nil {
			return nil, fmt.Errorf("that host key could not be parsed — paste the output of `ssh-keyscan <host>`: %w", err)
		}
		pinned = append(pinned, pub)
	}

	if len(pinned) == 0 {
		return nil, fmt.Errorf("no host key found — paste the output of `ssh-keyscan <host>`")
	}

	want := make([][]byte, 0, len(pinned))
	for _, p := range pinned {
		want = append(want, p.Marshal())
	}

	return func(_ string, _ net.Addr, presented ssh.PublicKey) error {
		got := presented.Marshal()
		for _, w := range want {
			if bytes.Equal(w, got) {
				return nil
			}
		}
		return fmt.Errorf("ssh: host key mismatch: the server presented a %s key that is not pinned on this credential",
			presented.Type())
	}, nil
}
