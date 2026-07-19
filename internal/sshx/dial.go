package sshx

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// This is the third Docker transport, after the local socket and the agent tunnel: Daffa dials
// OUT over SSH and opens a channel to the remote Docker endpoint, in process. There is no ssh
// binary in the image and no subprocess — the whole reason this is x/crypto/ssh and not Docker's
// own ssh:// connection helper, which shells out (docs/clusters.md §2).

// DialConfig is everything needed to reach a remote Docker daemon over SSH. The private key
// arrives already unsealed; nothing here touches the store.
type DialConfig struct {
	Host          string
	Port          int // 0 ⇒ 22
	User          string
	PrivateKeyPEM string
	Passphrase    string // for an encrypted key; "" otherwise
	// KnownHostKey pins the server. Empty means trust-on-first-use — the first dial records the
	// key it saw. Non-empty means the presented key must match exactly, or the dial is refused.
	KnownHostKey string
}

// ErrHostKeyChanged is returned when a pinned host key does not match what the server now
// presents — a deliberate re-pin, or a possible MITM. It is never re-pinned silently
// (docs/clusters.md §7); an operator re-adds the cluster to accept a rotated key.
var ErrHostKeyChanged = errors.New("sshx: the server's SSH host key changed since it was pinned")

// Connect dials the SSH host and returns a live client plus the pinned host-key line — the one
// that matched when KnownHostKey was set, or the newly-seen one on first use. The caller
// persists the returned line on first use.
func Connect(ctx context.Context, cfg DialConfig) (*ssh.Client, string, error) {
	signer, err := parseSigner(cfg.PrivateKeyPEM, cfg.Passphrase)
	if err != nil {
		return nil, "", err
	}

	port := cfg.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))

	var pinned string
	var mismatch bool
	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: pinningCallback(cfg.KnownHostKey, &pinned, &mismatch),
		Timeout:         15 * time.Second,
	}

	// ssh.Dial takes no context; run it in a goroutine so the caller's ctx can bound it —
	// the reconnect loop's cancellation and the request deadline both have to reach the dial.
	type result struct {
		c   *ssh.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ssh.Dial("tcp", addr, clientCfg)
		ch <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case r := <-ch:
		if mismatch {
			// The handshake error wraps the callback's, but the sentinel is what the caller
			// switches on, so return it unambiguously.
			return nil, "", ErrHostKeyChanged
		}
		if r.err != nil {
			return nil, "", fmt.Errorf("sshx: dialing %s@%s: %w", cfg.User, addr, r.err)
		}
		return r.c, pinned, nil
	}
}

// parseSigner turns an unsealed private key (and optional passphrase) into an SSH signer,
// mapping the encrypted-but-no-passphrase case to the same specific error the key store uses.
func parseSigner(privatePEM, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		s, err := ssh.ParsePrivateKeyWithPassphrase([]byte(privatePEM), []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("sshx: parsing private key: %w", err)
		}
		return s, nil
	}
	s, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if _, ok := err.(*ssh.PassphraseMissingError); ok {
		return nil, ErrPassphraseRequired
	}
	if err != nil {
		return nil, fmt.Errorf("sshx: parsing private key: %w", err)
	}
	return s, nil
}

// pinningCallback is the host-key TOFU rule: pin what the server presents on first use, and
// afterwards require an exact match — mirroring the agent's server-certificate pinning. The
// pinned form is the authorized_keys line (type + base64), independent of how the host is named.
func pinningCallback(known string, pinned *string, mismatch *bool) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		want := strings.TrimSpace(known)
		if want == "" {
			*pinned = got // trust on first use
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			*mismatch = true
			return ErrHostKeyChanged
		}
		*pinned = want
		return nil
	}
}

// SocketDialer returns a dockerx-style Dialer that opens a fresh channel to the remote Docker
// endpoint over the SSH client for each Docker API connection. The moby client cannot tell this
// from a local socket, which is the entire point (docs/clusters.md §2).
func SocketDialer(client *ssh.Client, endpoint string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	network, address := parseEndpoint(endpoint)
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return client.DialContext(ctx, network, address)
	}
}

// parseEndpoint splits a Docker endpoint into the network and address the SSH channel dials on
// the remote side. Defaults to the standard unix socket, which is what almost every host runs.
func parseEndpoint(endpoint string) (network, address string) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "unix", "/var/run/docker.sock"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "unix", "/var/run/docker.sock"
	}
	switch u.Scheme {
	case "unix":
		return "unix", u.Path
	case "tcp", "http", "https":
		return "tcp", u.Host
	default:
		return "unix", "/var/run/docker.sock"
	}
}
