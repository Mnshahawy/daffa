package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The latest-tag hint. It is a convenience — "the newest tag that looks like yours" — never a
// gate: any failure returns no hint, not an error the operator has to clear. To stay well under
// registry rate limits, the fetched tag list is cached per repository and reused across the
// images in a stack. See .ai/image-upgrades.md §3.2.

const tagCacheTTL = 10 * time.Minute

var (
	tagCacheMu sync.Mutex
	tagCache   = map[string]tagCacheEntry{}
)

type tagCacheEntry struct {
	tags []string
	at   time.Time
}

// LatestHint returns the highest tag that shares currentTag's shape (its variant suffix, e.g.
// "-alpine", and a dotted-numeric core) and ranks above it — or "" when there is nothing higher,
// nothing comparable, or the current tag itself is not a version (latest, a date, a sha). The
// boolean-free contract is deliberate: callers treat "" as "no hint".
func LatestHint(ctx context.Context, host, repo, currentTag, username, password string) (string, error) {
	if _, ok := parseVersionTag(currentTag); !ok {
		return "", nil // an unversioned current tag has nothing to be "newer" than — skip the fetch
	}
	tags, err := listTagsCached(ctx, host, repo, username, password)
	if err != nil {
		return "", err
	}
	return pickLatest(currentTag, tags), nil
}

// pickLatest is the heuristic, isolated from the network: the highest tag among candidates that
// shares currentTag's variant and outranks it, or "" if none does.
func pickLatest(currentTag string, tags []string) string {
	cur, ok := parseVersionTag(currentTag)
	if !ok {
		return ""
	}
	best, bestRaw := cur, ""
	for _, t := range tags {
		v, ok := parseVersionTag(t)
		if !ok || v.variant != cur.variant {
			continue
		}
		if v.greater(best) {
			best, bestRaw = v, t
		}
	}
	return bestRaw
}

// version is a tag split into a comparable numeric core and its variant suffix. "16.2-alpine"
// is {core:[16 2], variant:"alpine"}; a leading "v" is ignored so v1.2.3 and 1.2.3 compare.
type version struct {
	core    []int
	variant string
}

func parseVersionTag(tag string) (version, bool) {
	t := strings.TrimSpace(tag)
	variant := ""
	if i := strings.IndexByte(t, '-'); i >= 0 {
		variant, t = t[i+1:], t[:i]
	}
	t = strings.TrimPrefix(t, "v")
	if t == "" {
		return version{}, false
	}
	parts := strings.Split(t, ".")
	core := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > maxVersionComponent {
			// Not a pure numeric core — a sha or "latest" (parse error), or an undotted date
			// stamp like 20240101 (too large). The magnitude cap is what keeps a date tag from
			// ranking above real semver: calendar versions use dots (2024.1.15) and survive it.
			return version{}, false
		}
		core = append(core, n)
	}
	return version{core: core, variant: variant}, true
}

// A version component past this is almost certainly a date stamp or build number, not a semver
// field — no real release numbers a single component into six figures.
const maxVersionComponent = 100000

func (a version) greater(b version) bool {
	for i := 0; i < len(a.core) || i < len(b.core); i++ {
		av, bv := 0, 0
		if i < len(a.core) {
			av = a.core[i]
		}
		if i < len(b.core) {
			bv = b.core[i]
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}

func listTagsCached(ctx context.Context, host, repo, username, password string) ([]string, error) {
	key := host + "/" + repo

	tagCacheMu.Lock()
	if e, ok := tagCache[key]; ok && time.Since(e.at) < tagCacheTTL {
		tagCacheMu.Unlock()
		return e.tags, nil
	}
	tagCacheMu.Unlock()

	tags, err := listTags(ctx, host, repo, username, password)
	if err != nil {
		return nil, err
	}

	tagCacheMu.Lock()
	tagCache[key] = tagCacheEntry{tags: tags, at: time.Now()}
	tagCacheMu.Unlock()
	return tags, nil
}

// listTags fetches candidate tags cheaply. Docker Hub is special-cased onto its own API, which
// orders by recency and — crucially — lives off the pull-limit path, so the hint never spends the
// budget tag validation needs. Every other registry answers a single (capped) v2 tags list.
func listTags(ctx context.Context, host, repo, username, password string) ([]string, error) {
	if RegistryHost(host) == "docker.io" {
		return hubTags(ctx, repo)
	}
	return registryTagList(ctx, host, repo, username, password)
}

// hubTags reads the most recently pushed tags from hub.docker.com. Anonymous: a private Hub repo
// simply yields no hint, which is the correct degradation.
func hubTags(ctx context.Context, repo string) ([]string, error) {
	client := &http.Client{Timeout: 12 * time.Second}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	url := "https://hub.docker.com/v2/repositories/" + repo + "/tags?page_size=100&ordering=last_updated"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker hub answered %s", resp.Status)
	}

	var body struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(body.Results))
	for _, r := range body.Results {
		tags = append(tags, r.Name)
	}
	return tags, nil
}

// registryTagList reads one capped page of /v2/<repo>/tags/list. The v2 API does not order by
// recency, so this feeds the version heuristic rather than being trusted as "latest" itself.
func registryTagList(ctx context.Context, host, repo, username, password string) ([]string, error) {
	base := registryBaseURL(host)
	client := &http.Client{Timeout: 12 * time.Second}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := registryAuthedGet(ctx, client, base+"/v2/"+repo+"/tags/list?n=200", "application/json", username, password)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the registry answered %s listing tags", resp.Status)
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Tags, nil
}
