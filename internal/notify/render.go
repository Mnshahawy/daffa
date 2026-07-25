package notify

import (
	"bytes"
	"embed"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"
	"time"
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

// palette is the per-severity colour set, resolved in Go so the template carries no
// conditional colour logic. The hexes are the app's own state colours (brand/tokens.css)
// flattened to sRGB — email cannot use oklch or CSS variables.
type palette struct {
	accent string // the header bar and the metadata accent
	soft   string // the tinted detail block for a failure
	border string // the detail block border
}

var palettes = map[string]palette{
	"ok":     {accent: "#16a34a", soft: "#fafafa", border: "#e4e4e7"},
	"warn":   {accent: "#d97706", soft: "#fffbeb", border: "#fde68a"},
	"danger": {accent: "#dc2626", soft: "#fef2f2", border: "#fecaca"},
}

// mailView is Data plus everything the template would otherwise have to compute: the
// severity colours, the human timestamp, and the caption over the detail block. Keeping the
// logic here — not in the template — means it is testable and the template stays a layout.
type mailView struct {
	Data
	Accent      string
	DetailBG    string
	DetailBrdr  string
	DetailLabel string
	WhenText    string
	HasMeta     bool
}

func newView(d Data) mailView {
	p := palettes[d.severity()]
	// "Details", always. Tying the caption to severity ("Error output" for danger) lies for
	// the danger events whose detail is context, not a failure dump — break-glass carries the
	// caller's IP, a fired monitor carries the breached rule. Neutral is honest for all of them.
	label := "Details"
	when := ""
	if !d.When.IsZero() {
		// UTC and unambiguous: these land in inboxes in every timezone, and an operator
		// correlating a mail with a log wants one clock, not the sender's local one.
		when = d.When.UTC().Format("Jan 2, 2006 · 15:04 MST")
	}
	return mailView{
		Data:        d,
		Accent:      p.accent,
		DetailBG:    p.soft,
		DetailBrdr:  p.border,
		DetailLabel: label,
		WhenText:    when,
		HasMeta:     d.HostName != "" || d.Target != "" || when != "",
	}
}

// Render builds the mail for an event.
//
// One template, filled differently per event, rather than one template per event. The
// events all say the same four things — what happened, where, what the error was, and a way
// back into the console — and six near-identical files would drift apart within a year.
func Render(d Data) (Rendered, error) {
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "base", newView(d)); err != nil {
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

	// The same structured facts the HTML shows as a strip, as aligned rows a terminal mail
	// client renders cleanly. Only the fields that are set — a blank "Host:" line is noise.
	var meta [][2]string
	if d.HostName != "" {
		meta = append(meta, [2]string{"Host", d.HostName})
	}
	if d.Target != "" {
		meta = append(meta, [2]string{"Target", d.Target})
	}
	if !d.When.IsZero() {
		meta = append(meta, [2]string{"When", d.When.UTC().Format("Jan 2, 2006 · 15:04 MST")})
	}
	for _, m := range meta {
		fmt.Fprintf(&b, "%-8s%s\n", m[0]+":", m[1])
	}
	if len(meta) > 0 {
		b.WriteString("\n")
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

// withBaseURL turns an event's in-app path into the absolute link an email or channel shows,
// prepending the operator-configured public base URL. An empty base URL (Daffa sits behind a proxy
// and cannot know its own address) or an empty path yields no link, so the template omits the
// button rather than rendering one that goes nowhere. Send and the previews all go through here so
// a test email's "Open in Daffa" button points where a real one would.
func withBaseURL(baseURL, path string) string {
	if baseURL == "" || path == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + path
}

// Preview renders an event with plausible data for the settings-page preview, using a sample base
// URL so the button always shows regardless of whether one is configured.
func Preview(e Event) (Rendered, error) {
	return PreviewFor(e, "https://ops.example.com")
}

// PreviewFor renders an event with plausible data against a specific base URL — the test-email path
// passes the operator's real one, so the sent message's link matches what real notifications use
// (and is omitted when no base URL is configured, exactly as a real send would).
func PreviewFor(e Event, baseURL string) (Rendered, error) {
	d := Data{
		Event:    e,
		Subject:  "Deploy failed: billing on prod",
		Title:    "Deploy failed: billing",
		Summary:  "The deploy of stack “billing” exited with code 1.",
		HostName: "prod",
		Target:   "billing",
		Detail:   "billing-web  Pulling\nbilling-web  Error response from daemon: pull access denied for acme/billing-web",
		Link:     withBaseURL(baseURL, "/stacks/abc"),
		Failed:   true,
		// A fixed instant so the preview and its tests are deterministic — a real send stamps
		// this with the wall clock in notify.Send.
		When: time.Date(2026, time.July, 24, 20, 10, 0, 0, time.UTC),
	}

	switch e {
	case BackupFailed:
		d.Subject = "Backup failed: billing-db on prod"
		d.Title = "Backup failed: billing-db"
		d.Summary = "The backup job “billing-db” failed."
		d.Detail = "pg_dumpall: error: connection to server failed: FATAL: password authentication failed"
	case BackupSucceeded:
		d.Subject = "Backup succeeded: billing-db on prod"
		d.Title = "Backup succeeded: billing-db"
		d.Summary = "The backup job “billing-db” completed. 412 MB written."
		d.Detail = ""
		d.Failed = false
	case DeploySucceeded:
		d.Subject = "Deploy succeeded: billing on prod"
		d.Title = "Deploy succeeded: billing"
		d.Summary = "The deploy of stack “billing” completed."
		d.Detail = ""
		d.Failed = false
	case AgentOffline:
		d.Subject = "Host offline: prod"
		d.Title = "Host offline: prod"
		d.Summary = "The host “prod” stopped answering. Daffa cannot reach its Docker daemon."
		d.Detail = ""
		d.Target = "prod"
	case BreakGlassUsed:
		d.Subject = "Break-glass sign-in used"
		d.Title = "Break-glass sign-in used"
		d.Summary = "Somebody redeemed a break-glass token and signed in as an administrator."
		d.Detail = "from 203.0.113.7"
		// Fleet-level: no single host owns it.
		d.HostName = ""
		d.Target = ""
	case CertExpiring:
		d.Subject = "Certificate expiring: web-frontend"
		d.Title = "Certificate expiring: web-frontend"
		d.Summary = "The uploaded certificate “web-frontend” expires 2026-08-01. Daffa cannot renew it — upload a replacement."
		d.Detail = ""
		d.Target = "web-frontend"
		d.HostName = ""
		// Not a failure — an action. severity() renders this amber, the fix for the green
		// "all clear" it used to show about a certificate somebody has to replace by hand.
		d.Failed = false
	case CertRenewFailed:
		d.Subject = "Certificate renewal failed: web-frontend"
		d.Title = "Certificate renewal failed: web-frontend"
		d.Summary = "Daffa tried to renew “web-frontend” and could not. The current certificate is still valid until 2026-08-01; renewal will be retried hourly."
		d.Detail = "certs: the CA that issued this certificate holds no private key"
		d.Target = "web-frontend"
		d.HostName = ""
	case CertRenewed:
		d.Subject = "Certificate renewed: web-frontend"
		d.Title = "Certificate renewed: web-frontend"
		d.Summary = "Renewed “web-frontend”: now valid until 2027-08-16."
		d.Detail = ""
		d.Target = "web-frontend"
		d.HostName = ""
		d.Failed = false
	case CARotationDue:
		d.Subject = "CA rotation: internal-ca"
		d.Title = "CA rotation: internal-ca"
		d.Summary = "The CA “internal-ca” expires 2026-12-30."
		d.Detail = "Stage a successor now (rotate), distribute the new root while both are trusted, then activate."
		d.Target = "internal-ca"
		d.HostName = ""
		d.Failed = false
	case KeyringRotated:
		d.Subject = "Keyring rotated: orders-db"
		d.Title = "Keyring rotated: orders-db"
		d.Summary = "Rotated “orders-db” on its 30-day schedule. New data encrypts under the new version; every prior version stays readable."
		d.Detail = ""
		d.Target = "orders-db"
		d.HostName = ""
		d.Failed = false
	case KeyringRotateFailed:
		d.Subject = "Keyring rotation failed: orders-db"
		d.Title = "Keyring rotation failed: orders-db"
		d.Summary = "Daffa tried to rotate “orders-db” and could not. Consumers keep encrypting with the current version; rotation will be retried hourly."
		d.Detail = "delivering to daffa-keys: prod-2: the environment is not connected"
		d.Target = "orders-db"
		d.HostName = ""
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
