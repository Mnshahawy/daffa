package notify

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Mnshahawy/daffa/internal/mailer"
	"github.com/Mnshahawy/daffa/internal/store"
)

const (
	// MaxAttempts before a message is given up on and left as a dead letter.
	MaxAttempts = 6

	// Backoff doubles from a minute, capped at half an hour: 1m, 2m, 4m, 8m, 16m, 30m.
	// Long enough to ride out a restarting mail server, short enough that an alert about a
	// failing backup still arrives while it is worth reading.
	backoffBase = 1 * time.Minute
	backoffCap  = 30 * time.Minute

	// tick is how often the worker looks. Sending is not latency-critical — the deploy
	// already finished — and a tight loop against SQLite for the 99.9% of ticks that find
	// nothing is a waste of the disk.
	tick = 20 * time.Second

	batch = 20
)

// Worker drains the outbox.
//
// It sends OUTSIDE any transaction, which is the whole reason the outbox exists. Sending
// inside one holds a database lock for as long as a slow SMTP server takes; and a
// transaction that rolls back after the send has already gone out produces an email about an
// event that did not happen — worse than no email, because your monitoring now believes it.
func (n *Notifier) Worker(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.drain(ctx)
		}
	}
}

func (n *Notifier) drain(ctx context.Context) {
	due, err := n.store.DueMessages(ctx, batch)
	if err != nil {
		n.log.Error("notify: reading the outbox", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}

	// The SMTP transport is built once for the batch, and is allowed to be nil: email may be off
	// while channels are on. It is only NEEDED if the batch actually holds an email message, so a
	// build error is not fatal to the channel messages sitting next to it — see attempt.
	transport, cfg, err := n.Transport(ctx)
	if err != nil {
		n.log.Error("notify: building the SMTP transport", "err", err)
		// Fall through with a nil transport: channel messages can still go.
		transport = nil
	}
	client := channelHTTPClient()

	for _, m := range due {
		n.attempt(ctx, transport, cfg, client, m)
	}
}

func (n *Notifier) attempt(ctx context.Context, t mailer.Transport, cfg *store.SMTPSettings, client *http.Client, m *store.OutboxMessage) {
	// A per-message deadline: one unreachable server must not stall the whole batch behind
	// a TCP timeout that the OS is in no hurry to declare.
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var err error
	switch m.Kind {
	case "", "email":
		if t == nil {
			// Email was switched off (or its transport would not build) after this was queued.
			// Leave it pending rather than failing it — switching email back on should deliver
			// what is waiting, not find a pile of dead letters. A channel message in the same
			// batch is unaffected.
			return
		}
		err = t.Send(sendCtx, mailer.Message{
			From:     cfg.FromAddr,
			FromName: cfg.FromName,
			To:       m.To,
			Subject:  m.Subject,
			HTML:     m.HTML,
			Text:     m.Text,
		})
	default:
		err = n.deliverChannel(sendCtx, client, m)
	}

	if err == nil {
		// Delivery is AT-LEAST-ONCE, deliberately. The send happens, THEN the row is marked
		// sent; a crash in that window leaves it pending and it is delivered again next tick.
		// There is no idempotency key, so a recipient can occasionally get a duplicate — which
		// for an alert is the right side to err on: a second copy is a shrug, a lost one is the
		// outage you never heard about. Do not "fix" this by marking sent before the send.
		if err := n.store.MarkSent(ctx, m.ID); err != nil {
			n.log.Error("notify: marking sent", "id", m.ID, "err", err)
		}
		n.log.Info("notification sent", "event", m.Event, "kind", m.Kind, "to", m.To)
		return
	}

	attempts := m.Attempts + 1
	if attempts >= MaxAttempts {
		// Give up, but keep the row. A failed alert that vanishes is the worst outcome of
		// all: you would believe nothing had gone wrong.
		if err := n.store.MarkFailed(ctx, m.ID, attempts, time.Time{}, err.Error()); err != nil {
			n.log.Error("notify: marking failed", "id", m.ID, "err", err)
		}
		n.log.Error("notification gave up",
			"event", m.Event, "to", m.To, "attempts", attempts, "err", err)
		return
	}

	retryAt := time.Now().Add(backoff(attempts))
	if err := n.store.MarkFailed(ctx, m.ID, attempts, retryAt, err.Error()); err != nil {
		n.log.Error("notify: scheduling a retry", "id", m.ID, "err", err)
	}
	n.log.Warn("notification failed, will retry",
		"event", m.Event, "to", m.To, "attempt", attempts, "next", retryAt, "err", err)
}

// deliverChannel unseals the channel's URL and POSTs the rendered payload. The URL is opened
// here, at the last possible moment, so the plaintext secret exists only for the duration of one
// request and never touches the outbox row.
func (n *Notifier) deliverChannel(ctx context.Context, client *http.Client, m *store.OutboxMessage) error {
	ch, err := n.store.ChannelByID(ctx, m.ChannelID)
	if err != nil {
		// The channel was deleted after the message was queued. Retrying will not bring it back,
		// but the attempt/backoff path treats it like any failure and lets it become a dead letter
		// — which is the right place for "meant for a channel that no longer exists".
		return fmt.Errorf("channel unavailable: %w", err)
	}
	url, err := n.sealer.Open(ch.URLEnc)
	if err != nil {
		return fmt.Errorf("opening channel URL: %w", err)
	}
	return PostChannel(ctx, client, url, m.Text)
}

// backoff: 1m, 2m, 4m, 8m, 16m, then capped at 30m.
func backoff(attempts int) time.Duration {
	d := backoffBase << (attempts - 1)
	if d > backoffCap || d <= 0 { // <= 0 guards the shift overflowing
		return backoffCap
	}
	return d
}
