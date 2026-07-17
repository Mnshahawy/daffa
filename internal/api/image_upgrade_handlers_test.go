package api

import "testing"

// classifyImage is what decides whether a row can be upgraded, pinned, or only looked at. The
// three kinds drive the whole tab, so pin them here rather than discover a miscategorised image
// as a broken dropdown.
func TestClassifyImage(t *testing.T) {
	cases := []struct {
		image           string
		kind, repo, tag string
	}{
		{"postgres:16", "tag", "library/postgres", "16"},
		{"ghcr.io/acme/api:1.2.3", "tag", "acme/api", "1.2.3"},
		{"nginx", "tag", "library/nginx", "latest"},
		{"redis@sha256:0000000000000000000000000000000000000000000000000000000000000000", "digest", "library/redis", ""},
		{"nginx:", "interpolated", "", ""}, // an unset ${TAG} resolves to this
	}
	for _, c := range cases {
		got := classifyImage(c.image, "svc")
		if got.Kind != c.kind || got.Repo != c.repo || got.Tag != c.tag {
			t.Errorf("classifyImage(%q) = {kind:%s repo:%s tag:%s}; want {kind:%s repo:%s tag:%s}",
				c.image, got.Kind, got.Repo, got.Tag, c.kind, c.repo, c.tag)
		}
	}
}
