package stacks

import "testing"

// normalizeHost has to pull a bare host out of whatever lands on the clipboard when someone is
// wiring up a deploy — a clone URL, an SSH remote, a host:port. Getting this wrong sends the
// keyscan at the wrong name and pins nothing, so the guided path silently does not help.
func TestNormalizeHost(t *testing.T) {
	ok := map[string]string{
		"github.com":                     "github.com",
		"GitHub.com":                     "github.com",
		"  gitlab.com  ":                 "gitlab.com",
		"git@github.com":                 "github.com",
		"git@github.com:acme/repo.git":   "github.com",
		"https://github.com/acme/repo":   "github.com",
		"ssh://git@git.example.com:22/x": "git.example.com",
		"git.example.com:22":             "git.example.com",
		"bitbucket.org/acme":             "bitbucket.org",
	}
	for in, want := range ok {
		got, err := normalizeHost(in)
		if err != nil {
			t.Errorf("normalizeHost(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}

	for _, bad := range []string{"", "   ", "has space.com"} {
		if got, err := normalizeHost(bad); err == nil {
			t.Errorf("normalizeHost(%q) = %q, want an error", bad, got)
		}
	}
}
