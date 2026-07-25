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
	fmt.Fprintf(&b, "%s *%s*", severityEmoji(d), d.Title)
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
	blocks := []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": b.String()}},
	}
	// The host and target as a muted context line — chat has no metadata strip, so this is
	// where the structured facts land now that the sentence no longer repeats them.
	if ctx := contextLine(d); ctx != "" {
		blocks = append(blocks, map[string]any{
			"type":     "context",
			"elements": []map[string]any{{"type": "mrkdwn", "text": ctx}},
		})
	}
	return map[string]any{
		// A top-level text is the fallback shown in the notification and on old clients; the block
		// is what a modern client renders. Providing both is Slack's documented recommendation.
		"text":   d.Subject,
		"blocks": blocks,
	}
}

// Discord renders as an embed so the colour bar can carry the signal at a glance, the same
// green/amber/red the email header uses — a warning is amber, not the green it used to share
// with a success.
func discordPayload(d Data) map[string]any {
	desc := d.Summary
	if d.Detail != "" {
		desc += fmt.Sprintf("\n```\n%s\n```", truncateRunes(d.Detail, 3500))
	}
	if d.Link != "" {
		desc += fmt.Sprintf("\n[Open in Daffa](%s)", d.Link)
	}
	embed := map[string]any{
		"title":       truncateRunes(d.Title, 250),
		"description": truncateRunes(desc, 4000),
		"color":       severityColor(d),
	}
	// Host and target as inline fields — the strip's equivalent in an embed.
	var fields []map[string]any
	if d.HostName != "" {
		fields = append(fields, map[string]any{"name": "Host", "value": d.HostName, "inline": true})
	}
	if d.Target != "" {
		fields = append(fields, map[string]any{"name": "Target", "value": d.Target, "inline": true})
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}
	return map[string]any{"embeds": []map[string]any{embed}}
}

// A generic webhook gets the structured event, not a rendered string — the whole point of the
// generic kind is that the receiver decides the presentation. This is a stable contract, so the
// field names are explicit rather than a struct-tag reflection of Data's Go names.
func webhookPayload(d Data) map[string]any {
	return map[string]any{
		"event":    string(d.Event),
		"title":    d.Title,
		"summary":  d.Summary,
		"detail":   d.Detail,
		"host":     d.HostName,
		"target":   d.Target,
		"link":     d.Link,
		"failed":   d.Failed,     // kept for back-compat
		"severity": d.severity(), // ok | warn | danger — the three-state signal
	}
}

// contextLine is the host/target line chat shows in lieu of the email's metadata strip.
// Whichever fields are set, joined with a middot; empty when neither is (a fleet event).
func contextLine(d Data) string {
	var parts []string
	if d.HostName != "" {
		parts = append(parts, "Host: *"+d.HostName+"*")
	}
	if d.Target != "" {
		parts = append(parts, "Target: *"+d.Target+"*")
	}
	return strings.Join(parts, "  ·  ")
}

func severityEmoji(d Data) string {
	switch d.severity() {
	case "danger":
		return ":red_circle:"
	case "warn":
		return ":large_orange_circle:"
	default:
		return ":large_green_circle:"
	}
}

func severityColor(d Data) int {
	switch d.severity() {
	case "danger":
		return 0xdc2626
	case "warn":
		return 0xd97706
	default:
		return 0x16a34a
	}
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
