package dockerx

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/distribution/reference"
)

// manifestAccept lists the manifest and index media types a registry may answer with. Leaving
// the list/index types off makes a multi-arch image reply as though the tag were missing.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
}, ", ")

// RegistryHost normalises a registry reference — a bare host, or a URL with scheme and path — to
// a single comparable host, collapsing Docker Hub's several spellings to "docker.io". It is how
// an image's host is matched against a stored registry credential's URL.
func RegistryHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimSuffix(s, "/")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	switch s {
	case "docker.io", "index.docker.io", "registry.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return s
}

// ImageRef is a compose image string reduced to what tag operations need.
type ImageRef struct {
	Ref    string // the original string, e.g. "ghcr.io/acme/api:1.2"
	Host   string // normalised registry host, e.g. "docker.io", "ghcr.io"
	Repo   string // repository path, e.g. "library/postgres", "acme/api"
	Tag    string // the tag, or "" when the ref is digest-pinned
	Digest string // the digest, or "" when the ref is tagged
}

// ParseImageRef resolves a compose image string the way Docker does: a bare name is Docker Hub
// under library/, an untagged ref is :latest. It errors on anything reference cannot parse —
// which for our callers means an unresolved ${VAR}, to be surfaced as such rather than upgraded.
func ParseImageRef(image string) (ImageRef, error) {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(image))
	if err != nil {
		return ImageRef{}, err
	}
	ref := ImageRef{Ref: image, Host: RegistryHost(reference.Domain(named)), Repo: reference.Path(named)}
	if c, ok := named.(reference.Canonical); ok {
		ref.Digest = c.Digest().String()
	}
	if t, ok := named.(reference.Tagged); ok {
		ref.Tag = t.Tag()
	}
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = "latest"
	}
	return ref, nil
}

// SwapTag returns oldRef with its tag replaced by newTag, preserving the original registry and
// repository spelling (so `postgres:16` becomes `postgres:17`, not `docker.io/library/postgres:17`).
// It refuses a digest-pinned ref — there is no tag to change — and anything that will not parse.
func SwapTag(oldRef, newTag string) (string, error) {
	ref, err := ParseImageRef(oldRef)
	if err != nil {
		return "", err
	}
	if ref.Digest != "" {
		return "", fmt.Errorf("a digest-pinned image has no tag to change")
	}
	// A ref written with an explicit tag ends with ":<tag>"; swap that suffix. An untagged ref
	// (implicit :latest) does not, so append instead of trimming a suffix that is not there.
	if strings.HasSuffix(oldRef, ":"+ref.Tag) {
		return strings.TrimSuffix(oldRef, ":"+ref.Tag) + ":" + newTag, nil
	}
	return oldRef + ":" + newTag, nil
}

// TagExists reports whether repo:tag resolves to a manifest at host. It follows the same v2 auth
// dance as CheckRegistry, but with a pull-scoped token so it can actually read the manifest. A
// 404 is a clean "no such tag", not an error; only transport and auth failures are errors.
func TagExists(ctx context.Context, host, repo, tag, username, password string, roots *x509.CertPool) (bool, error) {
	base := registryBaseURL(host)
	client := registryClient(roots)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := registryAuthedGet(ctx, client, base+"/v2/"+repo+"/manifests/"+tag, manifestAccept, username, password)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, fmt.Errorf("the registry would not let Daffa read %s — check the registry credential", repo)
	default:
		return false, fmt.Errorf("the registry answered %s", resp.Status)
	}
}

// registryAuthedGet GETs a registry URL, following a Bearer or Basic 401 challenge exactly once.
// It is the shared v2 access path — the manifest read (tag validation) and the tags list (the
// latest-tag hint) differ only in URL and Accept header.
func registryAuthedGet(ctx context.Context, client *http.Client, url, accept, username, password string) (*http.Response, error) {
	do := func(authorization string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		return client.Do(req)
	}

	resp, err := do("")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge := resp.Header.Get("Www-Authenticate")
	resp.Body.Close()
	switch {
	case strings.HasPrefix(strings.ToLower(challenge), "bearer"):
		token, err := bearerToken(ctx, client, challenge, username, password)
		if err != nil {
			return nil, err
		}
		return do("Bearer " + token)
	case strings.HasPrefix(strings.ToLower(challenge), "basic"):
		return do(basicAuth(username, password))
	default:
		return nil, fmt.Errorf("the registry asked for an authentication scheme Daffa does not recognise (%q)", challenge)
	}
}
