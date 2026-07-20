package api

import (
	"context"
	_ "embed"
	"net/http"
	"time"

	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/sshx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// provisionScript installs Docker (and the Compose plugin) on a bare machine over SSH. It is the
// ONLY shell Daffa runs on a host it manages — everything else is the Docker API (docs/clusters.md
// §8, §11). Embedded so it ships in the binary and cannot drift from what the handler runs.
//
//go:embed provision.sh
var provisionScript string

// handleProvision streams the setup script to a machine over SSH and relays its output as
// server-sent events. It is gated by clusters.provision — running a root script on someone else's
// box is a strictly larger power than registering a connection to it (docs/clusters.md §14.3).
//
// Errors after the stream opens are `error` events, not HTTP codes: once the response is an
// event-stream there is no status line left to send.
func (s *Server) handleProvision(w http.ResponseWriter, r *http.Request) {
	var req sshClusterRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	sse, err := httpx.NewSSE(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Installs pull packages over the network and can take minutes; give it real headroom, but
	// bound it so a wedged apt lock cannot hold the stream open forever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()

	client, _, err := s.dialForRequest(ctx, req)
	if err != nil {
		_ = sse.Send("error", map[string]string{"message": friendlySSHError(err)})
		return
	}
	defer client.Close()

	s.audit(r.Context(), store.AuditEntry{
		Action: "cluster.provision", Target: req.Host, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"user": req.User, "event": "start"}),
	})

	runErr := sshx.RunScript(ctx, client, provisionScript, func(line string) {
		_ = sse.Send("log", map[string]string{"text": line})
	})
	if runErr != nil {
		_ = sse.Send("error", map[string]string{"message": runErr.Error()})
		s.audit(r.Context(), store.AuditEntry{
			Action: "cluster.provision", Target: req.Host, Outcome: "error",
			Detail: store.AuditDetail(map[string]string{"error": runErr.Error()}),
		})
		return
	}
	_ = sse.Send("end", map[string]any{"ok": true})
}
