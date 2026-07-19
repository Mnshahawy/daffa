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
