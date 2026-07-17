package api

import "testing"

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
