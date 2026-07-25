package notify

import (
	"strings"
	"testing"
)

func TestWithBaseURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://daffa.example.app", "/stacks/abc", "https://daffa.example.app/stacks/abc"},
		{"https://daffa.example.app/", "/stacks/abc", "https://daffa.example.app/stacks/abc"}, // trailing slash trimmed
		{"", "/stacks/abc", ""},               // no base → no link
		{"https://daffa.example.app", "", ""}, // no path → no link
	}
	for _, c := range cases {
		if got := withBaseURL(c.base, c.path); got != c.want {
			t.Errorf("withBaseURL(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

// The test email must render its "Open in Daffa" link against the operator's configured base URL —
// that was the bug: it used a hardcoded placeholder and ignored the setting.
func TestPreviewForUsesConfiguredBaseURL(t *testing.T) {
	msg, err := PreviewFor(BackupFailed, "https://daffa.amany.app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.HTML, "https://daffa.amany.app/stacks/abc") {
		t.Errorf("preview HTML does not link to the configured base URL:\n%s", msg.HTML)
	}
	if !strings.Contains(msg.Text, "https://daffa.amany.app/stacks/abc") {
		t.Errorf("preview text does not carry the configured base URL:\n%s", msg.Text)
	}
	if strings.Contains(msg.HTML, "ops.example.com") {
		t.Errorf("preview leaked the sample placeholder host:\n%s", msg.HTML)
	}
}

// With no base URL configured the button is omitted rather than pointing nowhere — same rule a real
// send follows.
func TestPreviewForOmitsLinkWithoutBaseURL(t *testing.T) {
	msg, err := PreviewFor(BackupFailed, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.HTML, "/stacks/abc") {
		t.Errorf("with no base URL the preview must not render a link:\n%s", msg.HTML)
	}
}

// The settings-page preview keeps its sample base URL so the button always shows.
func TestPreviewUsesSampleBaseURL(t *testing.T) {
	msg, err := Preview(BackupFailed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.HTML, "https://ops.example.com/stacks/abc") {
		t.Errorf("the settings preview should show a sample link:\n%s", msg.HTML)
	}
}

// The header colour is the first thing read at 2am, so it must not lie: a success is green,
// a failure is red, and a thing that needs a hand — a certificate Daffa cannot renew — is
// amber, NOT the green it used to share with a completed backup.
func TestSeverityColours(t *testing.T) {
	const (
		green = "#16a34a"
		amber = "#d97706"
		red   = "#dc2626"
	)
	cases := []struct {
		event Event
		want  string
	}{
		{DeploySucceeded, green},
		{BackupSucceeded, green},
		{DeployFailed, red},
		{BackupFailed, red},
		{AgentOffline, red},
		{CertExpiring, amber},  // the bug this fixed: was rendering green
		{CARotationDue, amber}, // ditto
		{CertRenewed, green},
		{MonitorFired, red}, // caller marks it a failure
	}
	for _, c := range cases {
		msg, err := Preview(c.event)
		if err != nil {
			t.Fatal(err)
		}
		bar := "background:" + c.want
		if !strings.Contains(msg.HTML, bar) {
			t.Errorf("%s: header bar is not %s\n%s", c.event, c.want, firstLines(msg.HTML, 40))
		}
		for _, other := range []string{green, amber, red} {
			if other != c.want && strings.Contains(msg.HTML, "height:4px;background:"+other) {
				t.Errorf("%s: header bar is %s, want %s", c.event, other, c.want)
			}
		}
	}
}

// Host, target and time ride on the event as structured fields; the mail shows them as a
// strip and the text alternative as aligned rows, rather than only woven into the sentence.
func TestMetadataStripAndText(t *testing.T) {
	msg, err := Preview(BackupSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Host", "prod", "Target", "billing-db", "When", "Jul 24, 2026"} {
		if !strings.Contains(msg.HTML, want) {
			t.Errorf("metadata strip is missing %q", want)
		}
	}
	if !strings.Contains(msg.Text, "Host:") || !strings.Contains(msg.Text, "Target:") ||
		!strings.Contains(msg.Text, "When:") {
		t.Errorf("plain text is missing the metadata rows:\n%s", msg.Text)
	}

	// A fleet-level event owns no host, so the strip must not print an empty Host row.
	bg, err := Preview(BreakGlassUsed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bg.Text, "Host:") {
		t.Errorf("break-glass is fleet-level and must not show a Host row:\n%s", bg.Text)
	}
}

// A danger event whose detail is context, not a failure dump, must not caption it "Error
// output" — break-glass carries the caller's IP, and calling that an error output reads as a
// bug. The caption is the neutral "Details" for every event.
func TestDetailLabelIsNeutral(t *testing.T) {
	bg, err := Preview(BreakGlassUsed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bg.HTML, "Error output") {
		t.Errorf("break-glass detail is context (an IP), not error output:\n%s", firstLines(bg.HTML, 60))
	}
	if !strings.Contains(bg.HTML, "from 203.0.113.7") {
		t.Errorf("break-glass should still show its detail")
	}
	// A real failure gets the same neutral caption — the compose log is "Details" too.
	df, _ := Preview(DeployFailed)
	if strings.Contains(df.HTML, "Error output") {
		t.Errorf("the detail caption should be neutral everywhere")
	}
}

// The host is a field now, not a clause: the trimmed summaries must not say "on <host>".
func TestSummariesDoNotRepeatHost(t *testing.T) {
	for _, e := range []Event{DeploySucceeded, DeployFailed, BackupSucceeded, BackupFailed} {
		msg, err := Preview(e)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(msg.Text, "on prod") {
			t.Errorf("%s summary still repeats the host:\n%s", e, msg.Text)
		}
	}
}

// The hidden preheader carries the summary, so the inbox line reads as prose rather than a
// scrape of "Daffa" and the first visible words.
func TestPreheaderCarriesSummary(t *testing.T) {
	msg, err := Preview(BackupSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(msg.HTML, "The backup job")
	j := strings.Index(msg.HTML, "<h1")
	if i < 0 || i > j {
		t.Errorf("the summary should appear in the preheader, before the visible title")
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
