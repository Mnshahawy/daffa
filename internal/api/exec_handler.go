package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// Exec is the most dangerous thing Daffa can do — a shell inside a container, on a
// host whose Docker socket is root. So: operator or above, always audited (on open
// AND on close, since the interesting part is the duration), and never available to a
// viewer.
//
// The wire protocol is deliberately tiny. Binary frames are raw terminal bytes in both
// directions; text frames are control messages from the browser (currently only
// resize). Anything else would mean parsing terminal data for escape sequences, which
// is how terminal proxies grow security holes.

type execControl struct {
	Type string `json:"type"` // "resize"
	Rows uint   `json:"rows,omitempty"`
	Cols uint   `json:"cols,omitempty"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	// A WebSocket upgrade is a GET, so this route cannot be told apart from a read by its
	// pattern and the route table has to leave it open. The capability is enforced here
	// instead — and it is containers.exec, NOT containers.edit: being trusted to restart a
	// container is not the same as being trusted with a root shell on the host it runs on.
	//
	// Checked at THIS host, not fleet-wide: a shell on staging is not a shell on prod.
	if !u.Caps.Has(caps.ContainersExec, r.PathValue("cluster")) {
		auth.Deny(w, r, u, caps.ContainersExec, s.recordDenial)
		return
	}

	node, ok := s.nodeForContainer(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	rows := uintParam(r, "rows", 24)
	cols := uintParam(r, "cols", 80)

	// The exec must outlive the request context: r.Context() is cancelled the moment
	// the handler returns, and we are about to hand control to the socket.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	sess, err := node.Exec(ctx, id, nil, rows, cols)
	if err != nil {
		s.audit(r.Context(), store.AuditEntry{
			EnvID: node.EnvID, Action: "container.exec", Target: id, Outcome: "error",
			Detail: store.AuditDetail(map[string]string{"error": err.Error()}),
		})
		if errors.Is(err, dockerx.ErrNoShell) {
			httpx.Fail(w, r, http.StatusBadRequest, "no_shell",
				"This container has no shell to attach to (it is probably a distroless or scratch image).")
			return
		}
		httpx.Fail(w, r, http.StatusBadGateway, "exec_failed", err.Error())
		return
	}
	defer sess.Close()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin only. Daffa serves the SPA itself, so a cross-origin upgrade is
		// never legitimate — and an unchecked one would let any page on the internet
		// open a root shell in the operator's browser session.
		OriginPatterns: nil,
	})
	if err != nil {
		slog.Warn("exec websocket upgrade failed", "err", err)
		return
	}
	defer conn.CloseNow()

	started := time.Now()
	s.audit(r.Context(), store.AuditEntry{
		EnvID: node.EnvID, Action: "container.exec", Target: id, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"event": "open"}),
	})
	defer func() {
		s.audit(context.WithoutCancel(r.Context()), store.AuditEntry{
			EnvID: node.EnvID, Action: "container.exec", Target: id, Outcome: "ok",
			Detail: store.AuditDetail(map[string]any{
				"event": "close", "seconds": int(time.Since(started).Seconds()),
			}),
		})
	}()

	// container → browser
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 8192)
		for {
			n, err := sess.Conn.Read(buf)
			if n > 0 {
				if err := conn.Write(ctx, websocket.MessageBinary, buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				// The shell exited (EOF) or the connection went away. Either way the
				// session is over; tell the browser so it can say so.
				if err != io.EOF {
					slog.Debug("exec read ended", "err", err)
				}
				_ = conn.Close(websocket.StatusNormalClosure, "shell exited")
				return
			}
		}
	}()

	// browser → container
readLoop:
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			break // the user closed the tab, or the socket broke
		}

		switch typ {
		case websocket.MessageBinary:
			if _, err := sess.Conn.Write(data); err != nil {
				// The shell FD is gone. End the session — a bare break here would only
				// leave the switch and keep re-writing to a dead FD until the other
				// goroutine happened to tear the socket down. Break the loop to cleanup.
				break readLoop
			}
		case websocket.MessageText:
			var ctl execControl
			if err := json.Unmarshal(data, &ctl); err != nil {
				continue // a malformed control frame is not worth killing the shell over
			}
			if ctl.Type == "resize" {
				if err := sess.Resize(ctx, ctl.Rows, ctl.Cols); err != nil {
					slog.Debug("exec resize failed", "err", err)
				}
			}
		}
	}

	cancel()
	<-done
}

func uintParam(r *http.Request, name string, def uint) uint {
	v, err := strconv.ParseUint(r.URL.Query().Get(name), 10, 16)
	if err != nil || v == 0 {
		return def
	}
	return uint(v)
}
