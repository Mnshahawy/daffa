package dockerx

import "testing"

// The heuristic is the fragile part of the hint, so pin its behaviour: same variant only, real
// numeric ordering (not lexical, where "9" beats "10"), and nothing for unversioned tags.
func TestPickLatest(t *testing.T) {
	tags := []string{
		"16", "16.2", "16.4", "17.0",
		"16-alpine", "16.4-alpine", "16.10-alpine",
		"latest", "stable", "bookworm", "20240101",
	}
	cases := []struct {
		current, want string
	}{
		{"16", "17.0"},                // highest bare-numeric core
		{"16.2", "17.0"},              // dotted core, same (empty) variant
		{"16-alpine", "16.10-alpine"}, // variant is respected: 16.10 > 16.4, and numeric not lexical
		{"16.4-alpine", "16.10-alpine"},
		{"17.0", ""},     // already the top → no hint
		{"latest", ""},   // unversioned → no hint
		{"20240101", ""}, // a date is not a version we rank
	}
	for _, c := range cases {
		if got := pickLatest(c.current, tags); got != c.want {
			t.Errorf("pickLatest(%q) = %q, want %q", c.current, got, c.want)
		}
	}
}

func TestParseVersionTag(t *testing.T) {
	ok := map[string]version{
		"16":          {core: []int{16}},
		"v1.2.3":      {core: []int{1, 2, 3}},
		"16.2-alpine": {core: []int{16, 2}, variant: "alpine"},
	}
	for in, want := range ok {
		got, ok := parseVersionTag(in)
		if !ok || got.variant != want.variant || len(got.core) != len(want.core) {
			t.Errorf("parseVersionTag(%q) = %+v, %v", in, got, ok)
			continue
		}
		for i := range want.core {
			if got.core[i] != want.core[i] {
				t.Errorf("parseVersionTag(%q) core = %v, want %v", in, got.core, want.core)
				break
			}
		}
	}
	for _, in := range []string{"latest", "stable", "bookworm", "20240101-git", "", "v"} {
		if _, ok := parseVersionTag(in); ok {
			t.Errorf("parseVersionTag(%q) parsed as a version; it should not", in)
		}
	}
}
