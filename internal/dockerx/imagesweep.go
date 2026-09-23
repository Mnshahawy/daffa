package dockerx

// The scheduled image sweep, done by hand instead of by ImagesPrune.
//
// `docker image prune -a --filter until=…` cannot be told "except these". Its only
// exclusion is `label!=…`, and a label can only be put on an image you BUILD — not on the
// runner image Daffa pulls by digest, and not on a vendor image somebody pinned. So a
// host whose CI runners start from a locally built image, idle between jobs, loses that
// image every night, and Daffa loses its own runner image on almost every sweep (nothing
// references it between deploys, and it was built months ago).
//
// Telling "built here" from "pulled" is not an option either: under the containerd image
// store (Docker's default since 29) a local build carries a repo@sha256 digest exactly
// like a pulled image, and the API records no provenance. What CAN be said is "keep images
// that look like this", which is what KeepImages is.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"
)

// KeepLabel exempts an image from the scheduled sweep when set to anything but "false".
// It is the exclusion for images the operator builds: `LABEL daffa.keep=true` travels with
// the image, so it survives a rename that a pattern would miss.
const KeepLabel = "daffa.keep"

// keepRule is one parsed KeepImages pattern.
type keepRule struct {
	digest string         // "sha256:…" when the pattern pins a digest; matched on digest alone
	re     *regexp.Regexp // otherwise a glob over the image's tags
}

// parseKeepRules turns the policy's patterns into matchers. The grammar is deliberately
// the one people already type into `docker pull`, with `*` added:
//
//	myorg/runner          every tag of that repository
//	myorg/runner:1.*      some of its tags
//	ghcr.io/myorg/*       everything under a prefix (`*` crosses `/`)
//	name@sha256:…         exactly that content, whatever it is tagged as
//
// Tags are compared as `docker images` prints them — Docker Hub images without the
// docker.io/library/ prefix — because that is what the operator is looking at when they
// write the pattern.
func parseKeepRules(patterns []string) []keepRule {
	rules := make([]keepRule, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.Index(p, "@"); i >= 0 {
			rules = append(rules, keepRule{digest: p[i+1:]})
			continue
		}
		// No tag means every tag. The tag separator is the last ':' AFTER the last '/',
		// so a registry port (localhost:5000/app) is not mistaken for one.
		if !strings.Contains(p[strings.LastIndex(p, "/")+1:], ":") {
			p += ":*"
		}
		quoted := strings.ReplaceAll(regexp.QuoteMeta(p), `\*`, `.*`)
		rules = append(rules, keepRule{re: regexp.MustCompile(`^` + quoted + `$`)})
	}
	return rules
}

func (r keepRule) matches(img image.Summary) bool {
	if r.digest != "" {
		if img.ID == r.digest {
			return true
		}
		for _, d := range img.RepoDigests {
			if strings.HasSuffix(d, "@"+r.digest) {
				return true
			}
		}
		return false
	}
	for _, t := range img.RepoTags {
		if r.re.MatchString(t) {
			return true
		}
	}
	return false
}

// sweepVerdict is why an image stays, or "" when it goes. A reason rather than a bool so
// the test can say which rule saved an image, not just that one did.
func sweepVerdict(img image.Summary, inUse map[string]bool, cutoff time.Time, rules []keepRule) string {
	if inUse[img.ID] {
		return "in use"
	}
	// Created is when the image was BUILT (see PruneOptions.MinAge), in daemon time.
	if !time.Unix(img.Created, 0).Before(cutoff) {
		return "younger than the age floor"
	}
	if v, ok := img.Labels[KeepLabel]; ok && v != "false" {
		return "labelled " + KeepLabel
	}
	for _, r := range rules {
		if r.matches(img) {
			return "matches a keep pattern"
		}
	}
	return ""
}

// realTags drops Docker's "<none>:<none>" placeholder for an untagged image.
func realTags(img image.Summary) []string {
	out := make([]string, 0, len(img.RepoTags))
	for _, t := range img.RepoTags {
		if t != "<none>:<none>" {
			out = append(out, t)
		}
	}
	return out
}

// sweepUnusedImages is the scheduled image prune: every image no container references,
// older than opts.MinAge, and not kept by a label or a KeepImages pattern.
func (e *Node) sweepUnusedImages(ctx context.Context, opts PruneOptions) (*PruneResult, error) {
	res := &PruneResult{Target: PruneImages}

	// The age floor is measured against the DAEMON's clock, for the reason pruneFilters
	// sends `until` as a duration: the daemon may be on a machine whose clock is not ours.
	daemonNow := time.Now()
	if info, err := e.Client.Info(ctx); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, info.SystemTime); err == nil {
			daemonNow = t
		}
	}
	cutoff := daemonNow.Add(-opts.MinAge)

	imgs, err := e.Client.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("dockerx: listing images on %s: %w", e.Name, err)
	}
	inUse, err := e.imagesInUse(ctx)
	if err != nil {
		return nil, err
	}
	rules := parseKeepRules(opts.KeepImages)

	before, beforeErr := e.imageLayersSize(ctx)
	var estimated int64
	var failures []string

	for _, img := range imgs {
		if sweepVerdict(img, inUse, cutoff, rules) != "" {
			continue
		}
		// By tag, never forced. Removing each name leaves Docker to delete the content
		// when the last one goes, and refuse — rather than untag — if a container started
		// from it since we looked. Force would also untag an image shared by several
		// repositories that one of them still needs. An untagged image goes by id.
		refs := realTags(img)
		if len(refs) == 0 {
			refs = []string{img.ID}
		}
		gone := false
		for _, ref := range refs {
			resp, err := e.Client.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true})
			if err != nil {
				// A conflict is an image another image or a new container depends on —
				// what ImagesPrune skips silently too. Anything else is worth reporting.
				if !errdefs.IsConflict(err) && !errdefs.IsNotFound(err) {
					failures = append(failures, fmt.Sprintf("%s: %v", ref, err))
				}
				break
			}
			for _, d := range resp {
				if d.Deleted != "" {
					gone = true
				}
			}
		}
		if gone {
			res.Deleted++
			res.Items = append(res.Items, shortID(img.ID))
			estimated += img.Size
		}
	}

	// Freed is what the disk actually got back, measured, not the sum of image sizes —
	// layers shared with a surviving image are not freed, and summing Size would report
	// them as though they were. The sum is the fallback when the daemon won't say.
	after, afterErr := e.imageLayersSize(ctx)
	switch {
	case beforeErr == nil && afterErr == nil && before >= after:
		res.Freed = uint64(before - after)
	case beforeErr != nil || afterErr != nil:
		res.Freed = uint64(estimated)
	}

	if len(failures) > 0 {
		return res, fmt.Errorf("dockerx: removing images on %s: %s", e.Name, strings.Join(failures, "; "))
	}
	return res, nil
}

func (e *Node) imageLayersSize(ctx context.Context) (int64, error) {
	du, err := e.Client.DiskUsage(ctx, ImageDiskUsageOptions())
	if err != nil {
		return 0, err
	}
	return du.LayersSize, nil
}
