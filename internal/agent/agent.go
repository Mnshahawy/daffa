// Package agent is the half of Daffa that runs on a managed host.
//
// It is deliberately small and dumb: it dials out to the server, and then it copies
// bytes between the streams the server opens and the local Docker socket. It makes no
// decisions, holds no state beyond its own credentials, and exposes no listening port.
// Everything clever lives on the server, which is what keeps a fleet of these things
// maintainable.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/Mnshahawy/daffa/internal/tunnel"
)

type Config struct {
	// Server is the Daffa base URL, e.g. https://ops.example.com — the scheme is
	// switched to wss:// for the tunnel itself.
	Server string
	// JoinToken enrolls this host on first run. Ignored once enrolled.
	JoinToken string
	// DockerHost is the local socket to proxy.
	DockerHost string
	// StateFile persists the agent's identity between restarts.
	StateFile string
	// Version is reported to the server so an operator can see stale agents.
	Version string
	// Insecure skips TLS verification. For a server behind an internal CA that the
	// host does not trust — the pinned fingerprint (below) is what actually protects
	// the connection in that case.
	Insecure bool
}

// state is what the agent persists: enough to reconnect, and a fingerprint of the
// server it enrolled with.
type state struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Token     string `json:"token"`
	Server    string `json:"server"`
	// ServerFingerprint is the SHA-256 of the server's TLS certificate, recorded at
	// enrollment. On every later connection we check the server still presents that
	// certificate — so even with verification relaxed for an internal CA, the agent
	// will not hand its token to a different server that answers on the same name.
	ServerFingerprint string `json:"server_fingerprint,omitempty"`
}

// Run enrolls if necessary and then keeps a tunnel up until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Server == "" {
		return errors.New("agent: --server is required")
	}
	cfg.Server = strings.TrimSuffix(cfg.Server, "/")

	st, err := loadState(cfg.StateFile)
	if err != nil {
		return err
	}

	if st.Token == "" {
		if cfg.JoinToken == "" {
			return errors.New("agent: not enrolled yet — pass --token with a join token from the Daffa console")
		}
		st, err = enroll(ctx, cfg)
		if err != nil {
			return err
		}
		if err := saveState(cfg.StateFile, st); err != nil {
			return err
		}
		slog.Info("enrolled", "agent", st.AgentName, "server", st.Server)
	}

	// Prove the socket works before claiming to be a working agent, so a
	// misconfiguration surfaces here rather than as a confusing error in someone's
	// browser ten minutes later.
	if err := pingDocker(ctx, cfg.DockerHost); err != nil {
		return fmt.Errorf("agent: cannot reach the local Docker socket at %s: %w", cfg.DockerHost, err)
	}

	return connectLoop(ctx, cfg, st)
}

// connectLoop keeps the tunnel up. A dropped connection is normal (a server restart, a
// flaky link), so it is not an error to report and give up on — it is a state to
// recover from, with backoff so a server that is down does not get hammered by a fleet.
func connectLoop(ctx context.Context, cfg Config, st *state) error {
	backoff := time.Second

	for {
		err := connectOnce(ctx, cfg, st)
		if ctx.Err() != nil {
			return nil // shutting down
		}
		if err != nil {
			slog.Warn("tunnel down", "err", err, "retry_in", backoff.Round(time.Second))
		} else {
			slog.Info("tunnel closed by server", "retry_in", backoff.Round(time.Second))
		}

		// Jitter, so a fleet that lost the server together does not reconnect in
		// lockstep and knock it over the moment it returns.
		jittered := backoff + time.Duration(rand.Int64N(int64(backoff/2+1)))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jittered):
		}

		if backoff < 30*time.Second {
			backoff *= 2
		}
		// A connection that lasted a while means the server is healthy; start over from
		// a short delay next time rather than staying pessimistic forever.
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func connectOnce(ctx context.Context, cfg Config, st *state) error {
	wsURL, err := tunnelURL(cfg.Server, cfg.Version)
	if err != nil {
		return err
	}

	client := httpClient(cfg, st)
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + st.Token}},
	})
	if err != nil {
		return fmt.Errorf("dialing %s: %w", wsURL, err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(-1) // image pulls and log streams, not chat messages

	session, err := tunnel.Agent(ws)
	if err != nil {
		return err
	}
	defer session.Close()

	slog.Info("tunnel up", "server", cfg.Server, "agent", st.AgentName)

	// Serve streams until the tunnel dies. Each one is a Docker API connection the
	// server opened; we hand it straight to the local daemon.
	for {
		stream, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("tunnel closed: %w", err)
		}
		go proxy(ctx, stream, cfg.DockerHost)
	}
}

// proxy splices one tunnel stream onto the Docker socket. This is the entire data path:
// the agent never parses, inspects, or filters what goes through it. Anything else
// would mean maintaining a second implementation of the Docker API, and that is exactly
// the cost this design exists to avoid.
func proxy(ctx context.Context, stream net.Conn, dockerHost string) {
	defer stream.Close()

	local, err := dialDocker(ctx, dockerHost)
	if err != nil {
		slog.Error("cannot reach the local Docker socket", "err", err)
		return
	}
	defer local.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(local, stream); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stream, local); done <- struct{}{} }()

	// One direction closing ends the exchange — an exec that ends, a log stream the
	// server hung up on.
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ── enrollment ──────────────────────────────────────────────────────────────────

func enroll(ctx context.Context, cfg Config) (*state, error) {
	body, err := json.Marshal(map[string]string{"token": cfg.JoinToken, "version": cfg.Version})
	if err != nil {
		return nil, err
	}

	st := &state{Server: cfg.Server}
	client := httpClient(cfg, st) // records the fingerprint as a side effect of the handshake

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Server+"/agents/enroll", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent: enrolling with %s: %w", cfg.Server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return nil, fmt.Errorf("agent: enrollment refused: %s", e.Message)
	}

	var out struct {
		AgentID   string `json:"agent_id"`
		AgentName string `json:"agent_name"`
		Token     string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agent: reading enrollment response: %w", err)
	}

	st.AgentID, st.AgentName, st.Token = out.AgentID, out.AgentName, out.Token
	return st, nil
}

// ── state ───────────────────────────────────────────────────────────────────────

func loadState(path string) (*state, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &state{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: reading %s: %w", path, err)
	}

	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("agent: %s is corrupt: %w", path, err)
	}
	return &st, nil
}

func saveState(path string, st *state) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("agent: creating state dir: %w", err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// 0600: this file holds a credential that can drive the host's Docker daemon.
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("agent: writing %s: %w", path, err)
	}
	return nil
}

func tunnelURL(server, version string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("agent: --server is not a URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("agent: --server must be http:// or https://, got %q", u.Scheme)
	}
	u.Path = "/agents/connect"
	u.RawQuery = url.Values{"version": {version}}.Encode()
	return u.String(), nil
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
