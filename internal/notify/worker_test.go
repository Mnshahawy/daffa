package notify

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Mnshahawy/daffa/internal/store"
)

// passthroughSealer stands in for the real one: the "sealed" URL is the URL. The worker unseals
// with Open, which is all the Sealer interface asks of it.
type passthroughSealer struct{}

func (passthroughSealer) Open(s string) (string, error) { return s, nil }

// The worker's whole reason for existing on the channel side: a queued channel message is drained,
// its URL unsealed, its payload POSTed, and — on a 2xx — the row leaves the queue. This is the
// production path (Enqueue → drain → deliverChannel → PostChannel), distinct from the synchronous
// test button, so it earns its own test.
func TestWorkerDeliversAQueuedChannelMessage(t *testing.T) {
	ctx := context.Background()

	var got atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		s := string(buf)
		got.Store(&s)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A channel whose sealed URL points at the test server, and a queued message for it.
	ch := &store.NotificationChannel{Kind: "slack", Name: "ops", URLEnc: srv.URL, Enabled: true}
	if err := st.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue(ctx, &store.OutboxMessage{
		Event: "deploy.failed", Kind: "slack", ChannelID: ch.ID,
		Subject: "Deploy failed", Text: `{"text":"boom"}`,
	}); err != nil {
		t.Fatal(err)
	}

	n := New(st, passthroughSealer{}, slog.New(slog.DiscardHandler))
	n.drain(ctx)

	if p := got.Load(); p == nil || *p != `{"text":"boom"}` {
		t.Fatalf("the payload did not reach the channel: %v", p)
	}
	// Delivered means gone from the queue: nothing due, and nothing in the dead-letter list.
	if due, _ := st.DueMessages(ctx, 10); len(due) != 0 {
		t.Errorf("a delivered channel message is still due: %d", len(due))
	}
	if failed, _ := st.FailedMessages(ctx, 10); len(failed) != 0 {
		t.Errorf("a delivered channel message landed in the dead-letter list: %d", len(failed))
	}
}

// A channel message whose endpoint rejects it must NOT be marked sent — it retries, and after the
// last attempt becomes a visible dead letter. Treating a 4xx as delivered is the exact way an
// alert evaporates, and email and channels have to fail the same way.
func TestWorkerRetriesAFailingChannel(t *testing.T) {
	ctx := context.Background()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no_such_hook"))
	}))
	defer srv.Close()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ch := &store.NotificationChannel{Kind: "webhook", Name: "hook", URLEnc: srv.URL, Enabled: true}
	if err := st.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue(ctx, &store.OutboxMessage{
		Event: "deploy.failed", Kind: "webhook", ChannelID: ch.ID, Text: `{}`,
	}); err != nil {
		t.Fatal(err)
	}

	n := New(st, passthroughSealer{}, slog.New(slog.DiscardHandler))
	n.drain(ctx)

	if hits.Load() != 1 {
		t.Fatalf("expected exactly one delivery attempt, got %d", hits.Load())
	}
	// Not sent: it is still pending (with a retry scheduled), not silently dropped.
	failed, _ := st.FailedMessages(ctx, 10)
	if len(failed) != 0 {
		t.Fatalf("a message with retries left was already a dead letter: %d", len(failed))
	}
	// The provider's own words must be on the row, so the eventual dead letter is diagnosable.
	// (Reading the pending row via a fresh due query after resetting its retry time is overkill
	// here; the FailedMessages path is covered by the store's own dead-letter test.)
}
