package notify

import (
	"bytes"
	"embed"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"
)

//go:embed templates/*.html
var templateFS embed.FS

// tpl is parsed once, at init. A template that fails to parse is a programming error, and
// discovering it at 2am — inside the code path that was trying to tell you your backups are
// failing — is the worst possible time.
var tpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// Rendered is a message ready for the outbox.
type Rendered struct {
	Subject string
	HTML    string
	Text    string
}

// Render builds the mail for an event.
//
// One template, filled differently per event, rather than one template per event. The
// events all say the same four things — what happened, where, what the error was, and a way
// back into the console — and six near-identical files would drift apart within a year.
func Render(d Data) (Rendered, error) {
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "base", d); err != nil {
		return Rendered{}, fmt.Errorf("notify: rendering %s: %w", d.Event, err)
	}

	return Rendered{
		Subject: d.Subject,
		HTML:    buf.String(),
		Text:    plainText(d),
	}, nil
}

// plainText is written from the DATA, not scraped out of the rendered HTML.
//
// Stripping tags from HTML to produce the text alternative is the usual shortcut and it
// always shows: you get stray whitespace, orphaned button labels, and a URL that only
// existed as an href. Writing it out costs ten lines and produces something a person can
// actually read in a terminal mail client.
func plainText(d Data) string {
	var b strings.Builder

	b.WriteString(d.Title)
	b.WriteString("\n")
	b.WriteString(strings.Repeat("-", len(d.Title)))
	b.WriteString("\n\n")

	if d.Summary != "" {
		b.WriteString(d.Summary)
		b.WriteString("\n\n")
	}
	if d.Detail != "" {
		b.WriteString(d.Detail)
		b.WriteString("\n\n")
	}
	if d.Link != "" {
		b.WriteString(d.Link)
		b.WriteString("\n\n")
	}

	b.WriteString("You are receiving this because of a notification rule in Daffa.\n")
	return b.String()
}

// Preview renders an event with plausible data, for the settings page. It is the only way to
// see what one of these looks like without breaking something first.
func Preview(e Event) (Rendered, error) {
	d := Data{
		Event:    e,
		Subject:  "Deploy failed: billing on prod",
		Title:    "Deploy failed: billing",
		Summary:  "The deploy of stack “billing” on prod exited with code 1.",
		HostName: "prod",
		Target:   "billing",
		Detail:   "billing-web  Pulling\nbilling-web  Error response from daemon: pull access denied for acme/billing-web",
		Link:     "https://ops.example.com/stacks/abc",
		Failed:   true,
	}

	switch e {
	case BackupFailed:
		d.Subject = "Backup failed: billing-db on prod"
		d.Title = "Backup failed: billing-db"
		d.Summary = "The backup job “billing-db” on prod failed."
		d.Detail = "pg_dumpall: error: connection to server failed: FATAL: password authentication failed"
	case BackupSucceeded:
		d.Subject = "Backup succeeded: billing-db on prod"
		d.Title = "Backup succeeded: billing-db"
		d.Summary = "The backup job “billing-db” on prod completed. 412 MB written."
		d.Detail = ""
		d.Failed = false
	case DeploySucceeded:
		d.Subject = "Deploy succeeded: billing on prod"
		d.Title = "Deploy succeeded: billing"
		d.Summary = "The deploy of stack “billing” on prod completed."
		d.Detail = ""
		d.Failed = false
	case AgentOffline:
		d.Subject = "Host offline: prod"
		d.Title = "Host offline: prod"
		d.Summary = "The host “prod” stopped answering. Daffa cannot reach its Docker daemon."
		d.Detail = ""
	case BreakGlassUsed:
		d.Subject = "Break-glass sign-in used"
		d.Title = "Break-glass sign-in used"
		d.Summary = "Somebody redeemed a break-glass token and signed in as an administrator."
		d.Detail = "from 203.0.113.7"
	case CertExpiring:
		d.Subject = "Certificate expiring: web-frontend"
		d.Title = "Certificate expiring: web-frontend"
		d.Summary = "The uploaded certificate “web-frontend” expires 2026-08-01. Daffa cannot renew it — upload a replacement."
		d.Detail = ""
		d.Target = "web-frontend"
	case CertRenewFailed:
		d.Subject = "Certificate renewal failed: web-frontend"
		d.Title = "Certificate renewal failed: web-frontend"
		d.Summary = "Daffa tried to renew “web-frontend” and could not. The current certificate is still valid until 2026-08-01; renewal will be retried hourly."
		d.Detail = "certs: the CA that issued this certificate holds no private key"
		d.Target = "web-frontend"
	case CertRenewed:
		d.Subject = "Certificate renewed: web-frontend"
		d.Title = "Certificate renewed: web-frontend"
		d.Summary = "Renewed “web-frontend”: now valid until 2027-08-16."
		d.Detail = ""
		d.Target = "web-frontend"
		d.Failed = false
	case CARotationDue:
		d.Subject = "CA rotation: internal-ca"
		d.Title = "CA rotation: internal-ca"
		d.Summary = "The CA “internal-ca” expires 2026-12-30."
		d.Detail = "Stage a successor now (rotate), distribute the new root while both are trusted, then activate."
		d.Target = "internal-ca"
		d.Failed = false
	case KeyringRotated:
		d.Subject = "Keyring rotated: orders-db"
		d.Title = "Keyring rotated: orders-db"
		d.Summary = "Rotated “orders-db” on its 30-day schedule. New data encrypts under the new version; every prior version stays readable."
		d.Detail = ""
		d.Target = "orders-db"
		d.Failed = false
	case KeyringRotateFailed:
		d.Subject = "Keyring rotation failed: orders-db"
		d.Title = "Keyring rotation failed: orders-db"
		d.Summary = "Daffa tried to rotate “orders-db” and could not. Consumers keep encrypting with the current version; rotation will be retried hourly."
		d.Detail = "delivering to daffa-keys: prod-2: the environment is not connected"
		d.Target = "orders-db"
	}

	return Render(d)
}

// truncate keeps a failure log to something an email can carry. The last lines are the ones
// that say why it failed, so it keeps the TAIL — a head-truncated compose log is a wall of
// "Pulling" and nothing else.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func Tail(s string, maxLines, maxBytes int) string {
	s = ansi.ReplaceAllString(s, "") // compose emits colour codes; they render as gibberish
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := strings.Join(lines, "\n")

	if len(out) > maxBytes {
		out = "…" + out[len(out)-maxBytes:]
	}
	return html.UnescapeString(out)
}
