// Package tunnel carries Docker API traffic from the server to an agent over a
// connection the AGENT opened.
//
// The direction matters. A managed host never listens for us: it dials out, which means
// no inbound port, no NAT traversal, and no Docker socket exposed to the network. But
// the server is the party that needs to make requests, so once the WebSocket is up the
// roles invert — the connection is multiplexed with yamux, and the SERVER opens streams
// down it while the agent accepts them.
//
// Each stream is one Docker API connection. That is the whole trick: the server's
// Docker client dials a yamux stream instead of a unix socket, and every feature Daffa
// already has (containers, logs, exec, stats) works against a remote host with no code
// that knows it is remote.
package tunnel

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// Keepalive has to be well inside the idle timeout of anything between the two ends —
// Traefik, cloudflared, a load balancer, a NAT table. Thirty seconds is comfortably
// under every default we care about, and the cost is two frames a minute.
const (
	KeepAliveInterval = 30 * time.Second
	connectionTimeout = 15 * time.Second
)

func config() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = KeepAliveInterval
	c.ConnectionWriteTimeout = connectionTimeout
	// yamux's own logger writes to stderr in a format that is not ours; silence it and
	// let the callers report what matters.
	c.LogOutput = nil
	c.Logger = nopLogger()
	return c
}

// Server wraps an accepted WebSocket (on the Daffa server) into a session that OPENS
// streams toward the agent.
func Server(ws *websocket.Conn) (*yamux.Session, error) {
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	session, err := yamux.Client(conn, config())
	if err != nil {
		return nil, fmt.Errorf("tunnel: starting session: %w", err)
	}
	return session, nil
}

// Agent wraps a dialed WebSocket (on the agent) into a session that ACCEPTS streams
// from the server.
func Agent(ws *websocket.Conn) (*yamux.Session, error) {
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	session, err := yamux.Server(conn, config())
	if err != nil {
		return nil, fmt.Errorf("tunnel: starting session: %w", err)
	}
	return session, nil
}

// Dialer returns a net dialer that opens a fresh stream per connection. This is what
// gets handed to the Docker client: it thinks it is dialing a socket.
func Dialer(session *yamux.Session) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		if session.IsClosed() {
			return nil, fmt.Errorf("tunnel: the agent is not connected")
		}
		stream, err := session.OpenStream()
		if err != nil {
			return nil, fmt.Errorf("tunnel: opening stream to agent: %w", err)
		}
		return stream, nil
	}
}
