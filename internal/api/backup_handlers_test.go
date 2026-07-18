package api

import "testing"

// sanitizeExcludePaths normalizes the volume exclude list and refuses anything that escapes the
// volume root. A pattern that escaped would match nothing at snapshot time, so silently keeping
// it would be a "backup" that ignored what the operator typed — hence a refusal, naming the raw
// pattern, rather than a quiet drop.
func TestSanitizeExcludePaths(t *testing.T) {
	ok := []struct {
		in, want string
	}{
		{"", ""},
		{"cache", "cache"},
		{"cache\ntmp/sessions", "cache\ntmp/sessions"},
		{"  cache  \n\n  logs \n", "cache\nlogs"}, // blanks and surrounding space dropped
		{"./cache/", "cache"},                     // leading ./ and trailing / folded away
		{"a//b/../b", "a/b"},                      // path.Clean collapses redundant segments
	}
	for _, c := range ok {
		got, bad := sanitizeExcludePaths(c.in)
		if bad != "" {
			t.Errorf("sanitizeExcludePaths(%q) unexpectedly rejected %q", c.in, bad)
			continue
		}
		if got != c.want {
			t.Errorf("sanitizeExcludePaths(%q) = %q; want %q", c.in, got, c.want)
		}
	}

	// Each of these must be refused, and the reported bad pattern is the raw line so the 400 can
	// point at exactly what the operator typed.
	bad := []struct{ in, wantBad string }{
		{"/etc/passwd", "/etc/passwd"},
		{"../secret", "../secret"},
		{"cache\n../escape", "../escape"}, // a good line does not excuse a bad one after it
		{"..", ".."},
		{".", "."},
	}
	for _, c := range bad {
		got, badPat := sanitizeExcludePaths(c.in)
		if badPat != c.wantBad {
			t.Errorf("sanitizeExcludePaths(%q) bad = %q; want %q", c.in, badPat, c.wantBad)
		}
		if got != "" {
			t.Errorf("sanitizeExcludePaths(%q) returned cleaned %q on rejection; want empty", c.in, got)
		}
	}
}

// keyUnderPrefix is the confinement that keeps a snapshot download inside its own job's prefix,
// so a caller holding backups.download on one job cannot pull another job's snapshots out of a
// shared bucket by naming their key. See handleSnapshotDownload.
func TestKeyUnderPrefix(t *testing.T) {
	cases := []struct {
		key, prefix string
		want        bool
	}{
		// Under the job's prefix: allowed.
		{"prod/2026-07-16/postgres-20260716T030000Z.age", "prod", true},
		{"prod/x.age", "prod/", true},     // trailing slash normalised away
		{"prod/x.age", "/prod/", true},    // leading slash too
		{"a/b/c/x.age", "a/b/c", true},    // nested prefix
		{"anything/at/all.age", "", true}, // empty prefix ⇒ the whole bucket is this job's
		{"top.age", "", true},

		// A sibling prefix in the same bucket: refused. This is the cross-job case.
		{"staging/db.age", "prod", false},
		{"prod-secrets/db.age", "prod", false}, // must match on the SEPARATOR, not a substring
		{"prod", "prod", false},                // the prefix dir itself is not an object under it

		// Traversal segments cannot be used to climb out of the claimed prefix.
		{"prod/../staging/db.age", "prod", false},
		{"../etc/passwd", "prod", false},
		{"prod/..", "prod", false},
	}
	for _, c := range cases {
		if got := keyUnderPrefix(c.key, c.prefix); got != c.want {
			t.Errorf("keyUnderPrefix(%q, %q) = %v; want %v", c.key, c.prefix, got, c.want)
		}
	}
}
