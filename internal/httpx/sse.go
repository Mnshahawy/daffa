package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SSE is a server-sent-events stream. Logs, stats and docker events all use it:
// it is one-directional (which is all they are), it survives proxies without any
// special configuration, and the browser reconnects on its own.
type SSE struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func NewSSE(w http.ResponseWriter, r *http.Request) (*SSE, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("httpx: response writer does not support flushing")
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Traefik and cloudflared both honor this; without it an intermediary may sit on
	// the stream waiting for a buffer to fill.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &SSE{w: w, flusher: flusher}, nil
}

// Send writes one named event with a JSON payload.
func (s *SSE) Send(event string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("httpx: marshaling SSE payload: %w", err)
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// Comment writes an SSE comment — used as a keepalive so idle streams are not
// reaped by an intermediary that has its own idle timeout.
func (s *SSE) Comment() error {
	if _, err := fmt.Fprint(s.w, ": ping\n\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// Keepalive pings every interval until the context is done. Run it in a goroutine
// alongside the producer; SSE.Send and SSE.Comment are not concurrency-safe, so pass
// a mutex-guarded writer if the producer is also writing. In practice Daffa's
// streams use the pattern in dockerx.StreamLogs: one writer, select on a ticker.
func (s *SSE) Keepalive(done <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := s.Comment(); err != nil {
				return
			}
		}
	}
}
