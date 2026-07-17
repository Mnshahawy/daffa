package stacks

import "testing"

func TestWatchPatternsDefaultsToTheComposeFile(t *testing.T) {
	// The default is the whole point: enabling auto-deploy without configuring anything
	// must watch the compose file and nothing else, or a README typo redeploys production.
	got := WatchPatterns("", "deploy/compose.yml")
	if len(got) != 1 || got[0] != "deploy/compose.yml" {
		t.Fatalf("WatchPatterns(\"\") = %v; want [deploy/compose.yml]", got)
	}

	got = WatchPatterns("compose/*.yml\nconfig/**\n", "ignored.yml")
	want := []string{"compose/*.yml", "config/**"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("WatchPatterns = %v; want %v", got, want)
	}
}

func TestMatches(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		changed  []string
		want     bool
	}{
		{"exact file", []string{"docker-compose.yml"}, []string{"docker-compose.yml"}, true},
		{"exact file, not touched", []string{"docker-compose.yml"}, []string{"README.md"}, false},
		{"nested exact", []string{"deploy/compose.yml"}, []string{"deploy/compose.yml"}, true},

		// * must not cross a separator, or "compose/*.yml" would match anything ending .yml
		{"star within a segment", []string{"compose/*.yml"}, []string{"compose/app.yml"}, true},
		{"star does not cross /", []string{"compose/*.yml"}, []string{"compose/sub/app.yml"}, false},
		{"star does not match a prefix elsewhere", []string{"*.yml"}, []string{"deploy/app.yml"}, false},

		{"doublestar crosses separators", []string{"config/**"}, []string{"config/a/b/c.conf"}, true},
		{"doublestar matches directly inside", []string{"config/**"}, []string{"config/app.conf"}, true},
		{"doublestar does not escape its root", []string{"config/**"}, []string{"other/app.conf"}, false},
		{"trailing slash means everything inside", []string{"config/"}, []string{"config/app.conf"}, true},

		{"any of several patterns", []string{"compose.yml", "config/**"}, []string{"README.md", "config/x"}, true},
		{"none of several patterns", []string{"compose.yml", "config/**"}, []string{"README.md", "src/main.go"}, false},

		{"leading slash on the changed path is tolerated", []string{"compose.yml"}, []string{"/compose.yml"}, true},
		{"question mark is one character", []string{"compose.y?l"}, []string{"compose.yml"}, true},

		{"nothing changed", []string{"compose.yml"}, nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Matches(tc.changed, tc.patterns); got != tc.want {
				t.Fatalf("Matches(%v, %v) = %v; want %v", tc.changed, tc.patterns, got, tc.want)
			}
		})
	}
}

// A pattern that fails to compile must not match everything — the failure mode of a
// broken filter has to be "deploys nothing", not "deploys on every push".
func TestBadPatternMatchesNothing(t *testing.T) {
	if Matches([]string{"anything.yml"}, []string{"["}) {
		t.Fatal("a malformed pattern matched; it must match nothing")
	}
}
