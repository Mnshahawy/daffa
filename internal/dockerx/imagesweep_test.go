package dockerx

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
)

// The keep patterns are the only thing standing between an operator's runner image and a
// 03:30 sweep, so their grammar is pinned case by case — especially the corners where a
// naive glob gets it wrong (a registry port read as a tag, `*` stopping at a slash).
func TestKeepRulesMatch(t *testing.T) {
	runnerDigest := "sha256:851f91d241214e7c6db86513b270d58776379aacc5eb9c4a87e5b47115e3065c"

	cases := []struct {
		pattern string
		img     image.Summary
		want    bool
	}{
		{"myorg/runner", image.Summary{RepoTags: []string{"myorg/runner:latest"}}, true},
		{"myorg/runner", image.Summary{RepoTags: []string{"myorg/runner:2024-01"}}, true},
		{"myorg/runner", image.Summary{RepoTags: []string{"myorg/runner-old:1"}}, false},
		{"myorg/runner:1.*", image.Summary{RepoTags: []string{"myorg/runner:1.4"}}, true},
		{"myorg/runner:1.*", image.Summary{RepoTags: []string{"myorg/runner:2.0"}}, false},
		{"ghcr.io/myorg/*", image.Summary{RepoTags: []string{"ghcr.io/myorg/ci/runner:x"}}, true},
		{"ghcr.io/myorg/*", image.Summary{RepoTags: []string{"ghcr.io/other/runner:x"}}, false},
		// A registry port is not a tag: without the "after the last slash" rule this would
		// become a literal match on "localhost:5000/app" and keep nothing.
		{"localhost:5000/app", image.Summary{RepoTags: []string{"localhost:5000/app:v3"}}, true},
		// Dots are literal, not regex wildcards.
		{"my.app", image.Summary{RepoTags: []string{"myxapp:1"}}, false},
		// A digest pin matches by content, whatever it is (or is not) tagged as — which is
		// how Daffa's own runner image, pulled by digest and untagged, is kept.
		{"docker:27-cli@" + runnerDigest, image.Summary{RepoDigests: []string{"docker@" + runnerDigest}}, true},
		{"docker:27-cli@" + runnerDigest, image.Summary{ID: runnerDigest}, true},
		{"docker:27-cli@" + runnerDigest, image.Summary{RepoTags: []string{"docker:27-cli"},
			RepoDigests: []string{"docker@sha256:0000"}}, false},
	}
	for _, c := range cases {
		rules := parseKeepRules([]string{c.pattern})
		if got := rules[0].matches(c.img); got != c.want {
			t.Errorf("%q vs %v%v: matched=%v, want %v", c.pattern, c.img.RepoTags, c.img.RepoDigests, got, c.want)
		}
	}
}

func TestSweepVerdict(t *testing.T) {
	now := time.Date(2026, 9, 22, 3, 30, 0, 0, time.UTC)
	cutoff := now.Add(-168 * time.Hour)
	old := now.Add(-30 * 24 * time.Hour).Unix()
	rules := parseKeepRules([]string{"myorg/runner"})

	cases := []struct {
		name  string
		img   image.Summary
		inUse bool
		keep  bool
	}{
		{"old, unused, unmatched goes", image.Summary{ID: "a", Created: old, RepoTags: []string{"app:1"}}, false, false},
		{"in use stays", image.Summary{ID: "b", Created: old, RepoTags: []string{"app:1"}}, true, true},
		{"younger than the floor stays", image.Summary{ID: "c", Created: now.Add(-time.Hour).Unix()}, false, true},
		{"labelled stays", image.Summary{ID: "d", Created: old, Labels: map[string]string{KeepLabel: "true"}}, false, true},
		{"labelled false goes", image.Summary{ID: "e", Created: old, Labels: map[string]string{KeepLabel: "false"}}, false, false},
		{"pattern stays", image.Summary{ID: "f", Created: old, RepoTags: []string{"myorg/runner:latest"}}, false, true},
		// One matching tag keeps the whole image: removing its other names would leave it
		// on disk anyway, and the operator asked for this content to stay.
		{"any matching tag keeps it", image.Summary{ID: "g", Created: old,
			RepoTags: []string{"scratch/tmp:1", "myorg/runner:ci"}}, false, true},
	}
	for _, c := range cases {
		inUse := map[string]bool{c.img.ID: c.inUse}
		got := sweepVerdict(c.img, inUse, cutoff, rules) != ""
		if got != c.keep {
			t.Errorf("%s: kept=%v, want %v", c.name, got, c.keep)
		}
	}
}
