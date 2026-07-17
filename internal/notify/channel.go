package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file is the non-email half of notify: turning a Data into the JSON a chat provider wants,
// and POSTing it. Email has SMTP and a worker that speaks it; a channel has a URL and this.

// RenderChannel produces the request body for one channel kind. It returns the bytes to POST,
// already provider-shaped — Slack and Discord each have their own idea of what a message is, and a
// generic webhook gets the event itself so the receiver can shape it however it likes.
//
// The payload is what lands in the outbox, so it must be self-contained: the worker delivers it
// verbatim, and by then the Data that produced it is long gone.
func RenderChannel(kind string, d Data) (string, error) {
	var payload any
	switch kind {
	case "slack":
		payload = slackPayload(d)
	case "discord":
		payload = discordPayload(d)
	case "webhook":
		payload = webhookPayload(d)
	default:
		return "", fmt.Errorf("notify: %q is not a channel kind", kind)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("notify: encoding %s payload: %w", kind, err)
	}
	return string(b), nil
}

// Slack renders as one section of mrkdwn plus a context line, which is the format that survives
// both the desktop app and the notification popover without looking like a robot pasted JSON.
func slackPayload(d Data) map[string]any {
	var b strings.Builder
	fmt.Fprintf(&b, "%s *%s*", statusEmoji(d.Failed), d.Title)
	if d.Summary != "" {
		fmt.Fprintf(&b, "\n%s", d.Summary)
	}
	if d.Detail != "" {
		// Slack code fences are ``` — the detail is an error log, and a fixed-width block is the
		// only way it stays legible.
		fmt.Fprintf(&b, "\n```%s```", truncateRunes(d.Detail, 2500))
	}
	if d.Link != "" {
		fmt.Fprintf(&b, "\n<%s|Open in Daffa>", d.Link)
	}
	return map[string]any{
		// A top-level text is the fallback shown in the notification and on old clients; the block
		// is what a modern client renders. Providing both is Slack's documented recommendation.
		"text": d.Subject,
		"blocks": []map[string]any{
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": b.String()}},
		},
	}
}

// Discord renders as an embed so the colour bar can carry the pass/fail signal at a glance, the
// same red/green the email header uses.
func discordPayload(d Data) map[string]any {
	desc := d.Summary
	if d.Detail != "" {
		desc += fmt.Sprintf("\n```\n%s\n```", truncateRunes(d.Detail, 3500))
	}
	if d.Link != "" {
		desc += fmt.Sprintf("\n[Open in Daffa](%s)", d.Link)
	}
	color := 0x2ecc71 // green
	if d.Failed {
		color = 0xe74c3c // red
	}
	return map[string]any{
		"embeds": []map[string]any{{
			"title":       truncateRunes(d.Title, 250),
			"description": truncateRunes(desc, 4000),
			"color":       color,
		}},
	}
}

// A generic webhook gets the structured event, not a rendered string — the whole point of the
// generic kind is that the receiver decides the presentation. This is a stable contract, so the
// field names are explicit rather than a struct-tag reflection of Data's Go names.
func webhookPayload(d Data) map[string]any {
	return map[string]any{
		"event":   string(d.Event),
		"title":   d.Title,
		"summary": d.Summary,
		"detail":  d.Detail,
		"host":    d.HostName,
		"target":  d.Target,
		"link":    d.Link,
		"failed":  d.Failed,
	}
}

func statusEmoji(failed bool) string {
	if failed {
		return ":red_circle:"
	}
	return ":large_green_circle:"
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// PostChannel delivers a rendered payload to a channel URL. It is deliberately strict about what
// counts as success: a chat webhook that returns 4xx/5xx has NOT delivered the message, and
// treating a 500 as "sent" is how an alert silently evaporates.
//
// The http.Client is passed in so the worker can reuse one across the batch and a test can inject
// a stub.
func PostChannel(ctx context.Context, client *http.Client, url, payload string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(payload)))
	if err != nil {
		return fmt.Errorf("notify: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: posting to channel: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Read a little of the body: providers explain the rejection there (Slack says "invalid_token",
	// Discord says which field is wrong), and that sentence in the dead-letter row is the whole
	// difference between a fixable error and a mystery.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("notify: channel returned %s", resp.Status)
	}
	return fmt.Errorf("notify: channel returned %s: %s", resp.Status, msg)
}

// channelHTTPClient is the worker's shared client. A short timeout: a chat webhook that takes more
// than ten seconds is down, and the per-message context deadline is the real backstop anyway.
func channelHTTPClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }
