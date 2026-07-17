package notify

import (
	"context"
	"log/slog"
	"strings"

	"github.com/Mnshahawy/daffa/internal/mailer"
	"github.com/Mnshahawy/daffa/internal/store"
)

// Sealer opens the sealed SMTP password. The config package provides it; the interface keeps
// this package from importing config.
type Sealer interface {
	Open(string) (string, error)
}

// Notifier turns an event into outbox rows.
type Notifier struct {
	store  *store.Store
	sealer Sealer
	log    *slog.Logger
}

func New(st *store.Store, sealer Sealer, log *slog.Logger) *Notifier {
	return &Notifier{store: st, sealer: sealer, log: log}
}

// Send resolves the recipients for an event and enqueues one message each.
//
// It does NOT send — the worker does that, outside any transaction. And it never returns an
// error to its caller: a notification that could not be queued must not fail the deploy it
// was about. The failure is logged, loudly, and that is the right trade — the alternative is
// an outage caused by the thing that was supposed to tell you about outages.
func (n *Notifier) Send(ctx context.Context, envID string, d Data) {
	cfg, err := n.store.SMTPSettings(ctx)
	if err != nil {
		n.log.Error("notify: reading SMTP settings", "err", err)
		return
	}

	// The link is only as good as the base URL somebody configured. Daffa sits behind a proxy
	// and cannot know its own public address, so a missing one omits the button rather than
	// rendering a link to nowhere. BaseURL lives in the SMTP row but belongs to every channel —
	// it is read even when email is off, because a Slack message wants the link too.
	if cfg.BaseURL != "" && d.Link != "" {
		d.Link = strings.TrimRight(cfg.BaseURL, "/") + d.Link
	} else {
		d.Link = ""
	}

	// Email and channels are resolved and enqueued independently: email is off on a fresh
	// install and channels are the whole reason this exists, so gating channels on an SMTP
	// server nobody configured would defeat the point.
	queued := n.queueEmail(ctx, envID, cfg, d)
	queued += n.queueChannels(ctx, string(d.Event), d)

	if queued > 0 {
		n.log.Info("notification queued", "event", d.Event, "messages", queued)
	}
}

// queueEmail enqueues one message per resolved email recipient. Returns how many.
func (n *Notifier) queueEmail(ctx context.Context, envID string, cfg *store.SMTPSettings, d Data) int {
	if !cfg.Enabled || cfg.Host == "" {
		return 0 // email is off; not an error
	}
	to, err := n.store.RecipientsFor(ctx, string(d.Event), envID)
	if err != nil {
		n.log.Error("notify: resolving recipients", "event", d.Event, "err", err)
		return 0
	}
	if len(to) == 0 {
		return 0
	}
	msg, err := Render(d)
	if err != nil {
		n.log.Error("notify: rendering", "event", d.Event, "err", err)
		return 0
	}
	queued := 0
	for _, addr := range to {
		if err := n.store.Enqueue(ctx, &store.OutboxMessage{
			Event: string(d.Event), Kind: "email", To: addr,
			Subject: msg.Subject, HTML: msg.HTML, Text: msg.Text,
		}); err != nil {
			n.log.Error("notify: enqueuing", "event", d.Event, "to", addr, "err", err)
			continue
		}
		queued++
	}
	return queued
}

// queueChannels enqueues one message per channel the event routes to, each with its provider's
// payload already rendered — the worker delivers the payload verbatim, so it has to be shaped now.
func (n *Notifier) queueChannels(ctx context.Context, event string, d Data) int {
	channels, err := n.store.ChannelsFor(ctx, event)
	if err != nil {
		n.log.Error("notify: resolving channels", "event", event, "err", err)
		return 0
	}
	queued := 0
	for _, c := range channels {
		payload, err := RenderChannel(c.Kind, d)
		if err != nil {
			n.log.Error("notify: rendering channel payload", "event", event, "channel", c.Name, "err", err)
			continue
		}
		if err := n.store.Enqueue(ctx, &store.OutboxMessage{
			Event: event, Kind: c.Kind, ChannelID: c.ID,
			Subject: d.Subject, Text: payload,
		}); err != nil {
			n.log.Error("notify: enqueuing channel", "event", event, "channel", c.Name, "err", err)
			continue
		}
		queued++
	}
	return queued
}

// Transport builds the SMTP transport from the stored settings, unsealing the password.
// Returns nil when email is not configured — which is a normal state, not an error.
func (n *Notifier) Transport(ctx context.Context) (mailer.Transport, *store.SMTPSettings, error) {
	cfg, err := n.store.SMTPSettings(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Enabled || cfg.Host == "" {
		return nil, cfg, nil
	}

	pw := ""
	if cfg.PasswordEnc != "" {
		if pw, err = n.sealer.Open(cfg.PasswordEnc); err != nil {
			return nil, cfg, err
		}
	}

	return &mailer.SMTP{
		Host: cfg.Host, Port: cfg.Port, Username: cfg.Username, Password: pw,
	}, cfg, nil
}
