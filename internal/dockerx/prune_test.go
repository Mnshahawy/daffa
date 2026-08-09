package dockerx

import (
	"testing"
	"time"
)

// The whole safety of the scheduled cleanup is in these two filter values, so they are
// asserted on the wire rather than trusted to read correctly at the call site. The failure
// this guards is silent: "dangling=false" without an "until" is `docker image prune -a`,
// which deletes the image of the release that went out this morning and reports success.
func TestPruneFilters(t *testing.T) {
	week := 168 * time.Hour

	cases := []struct {
		name   string
		target PruneTarget
		opts   PruneOptions
		want   map[string]string // key -> the single expected value; "" means the key must be absent
	}{
		{
			// The manual button: dangling images only, everything eligible regardless of age.
			name:   "manual image prune stays dangling-only",
			target: PruneImages, opts: PruneOptions{},
			want: map[string]string{"dangling": "true", "until": ""},
		},
		{
			// The scheduled sweep: every unused image, but only those older than a week.
			name:   "scheduled image prune widens and floors",
			target: PruneImages, opts: PruneOptions{MinAge: week, UnusedImages: true},
			want: map[string]string{"dangling": "false", "until": "168h0m0s"},
		},
		{
			// UnusedImages without an age floor is the dangerous combination. It is still
			// representable — the store's validation, not this layer, decides policy — but
			// it must at least be spelled exactly as asked.
			name:   "unused images without an age floor sends no until",
			target: PruneImages, opts: PruneOptions{UnusedImages: true},
			want: map[string]string{"dangling": "false", "until": ""},
		},
		{
			// Containers carry the age floor and nothing else: the dangling filter is an
			// image concept and Docker rejects it here.
			name:   "containers take the age floor only",
			target: PruneContainers, opts: PruneOptions{MinAge: week, UnusedImages: true},
			want: map[string]string{"dangling": "", "until": "168h0m0s"},
		},
		{
			name:   "networks take the age floor only",
			target: PruneNetworks, opts: PruneOptions{MinAge: 24 * time.Hour},
			want: map[string]string{"dangling": "", "until": "24h0m0s"},
		},
		{
			name:   "build cache takes the age floor only",
			target: PruneBuildCache, opts: PruneOptions{MinAge: week},
			want: map[string]string{"dangling": "", "until": "168h0m0s"},
		},
		{
			// Docker's volume prune has no `until`, and a filter the daemon rejects fails
			// the whole call — so the age floor is dropped rather than passed on.
			name:   "volumes never carry until",
			target: PruneVolumes, opts: PruneOptions{MinAge: week},
			want: map[string]string{"dangling": "", "until": ""},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := pruneFilters(c.target, c.opts)
			for key, want := range c.want {
				got := args.Get(key)
				if want == "" {
					if len(got) != 0 {
						t.Errorf("%s filter = %v; want it absent", key, got)
					}
					continue
				}
				if len(got) != 1 || got[0] != want {
					t.Errorf("%s filter = %v; want [%s]", key, got, want)
				}
			}
		})
	}
}
