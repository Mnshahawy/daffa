package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpClient builds the transport the agent uses for BOTH enrollment and the tunnel.
//
// The interesting part is the certificate pinning. Daffa is typically reached over an
// internal CA (or a tunnel) that a freshly-provisioned host does not trust, so the
// obvious move is --insecure — and the obvious move is wrong on its own, because it
// accepts any certificate at all, and the agent is about to hand over a token that
// drives the host's Docker daemon.
//
// So: on enrollment we record the fingerprint of whatever certificate the server
// presented. On every connection after that, we require the SAME certificate. The
// first connection is trust-on-first-use; every later one is pinned. That gives a host
// behind an internal CA the security property that matters (you are still talking to
// the server you enrolled with) without asking anyone to distribute a CA bundle.
func httpClient(cfg Config, st *state) *http.Client {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.Insecure || st.ServerFingerprint != "" {
		// Take verification into our own hands. Go still does the handshake; we decide
		// whether to accept the peer.
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyPeerCertificate = pinVerifier(cfg, st)
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			TLSHandshakeTimeout: 15 * time.Second,
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		},
		// No global timeout: the tunnel is a long-lived connection, and a Client
		// timeout would guillotine it mid-session.
	}
}

func pinVerifier(cfg Config, st *state) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("agent: server presented no certificate")
		}
		got := fingerprint(rawCerts[0])

		if st.ServerFingerprint == "" {
			// First contact (enrollment). Trust it, and remember it.
			st.ServerFingerprint = got
			slog.Info("pinned the server certificate", "fingerprint", got)
			return nil
		}

		if got != st.ServerFingerprint {
			return fmt.Errorf(
				"agent: the server's certificate changed (expected %s, got %s) — "+
					"if this is a legitimate certificate rotation, re-enroll this agent; "+
					"otherwise something is impersonating %s",
				st.ServerFingerprint, got, cfg.Server)
		}
		return nil
	}
}

// ── docker socket ───────────────────────────────────────────────────────────────

// dialDocker opens the local daemon, whatever form it takes on this host.
func dialDocker(ctx context.Context, host string) (net.Conn, error) {
	var d net.Dialer

	switch {
	case strings.HasPrefix(host, "unix://"):
		return d.DialContext(ctx, "unix", strings.TrimPrefix(host, "unix://"))
	case strings.HasPrefix(host, "tcp://"):
		u, err := url.Parse(host)
		if err != nil {
			return nil, fmt.Errorf("agent: bad docker host %q: %w", host, err)
		}
		return d.DialContext(ctx, "tcp", u.Host)
	case strings.HasPrefix(host, "/"):
		return d.DialContext(ctx, "unix", host)
	default:
		return nil, fmt.Errorf("agent: unsupported docker host %q (want unix:// or tcp://)", host)
	}
}

// pingDocker checks the socket is actually usable, so a bad --docker-host fails at
// startup with a clear message instead of turning into a broken environment in the UI.
func pingDocker(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := dialDocker(ctx, host)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialDocker(ctx, host)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the daemon answered %s (is this user in the docker group?)", resp.Status)
	}
	return nil
}
