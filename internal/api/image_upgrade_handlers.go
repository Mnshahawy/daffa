package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/stacks"
)

// Image upgrades for inline compose. The tab reads the YAML in the editor, lists the unique
// images, and lets the operator set and validate a new tag per image before Apply rewrites them
// back. See .ai/image-upgrades.md. These endpoints are non-mutating (StacksView); Apply is the
// edit (StacksEdit), and lives in the rewrite handler.

// previewImage is one distinct image in a compose file, classified by whether its tag can be
// upgraded at all.
type previewImage struct {
	Ref      string   `json:"ref"`              // the image string as written
	Services []string `json:"services"`         // the services that use it
	Kind     string   `json:"kind"`             // tag | digest | interpolated
	Host     string   `json:"host,omitempty"`   // normalised registry host, for a `tag`
	Repo     string   `json:"repo,omitempty"`   // repository path, for a `tag`
	Tag      string   `json:"tag,omitempty"`    // current tag, for a `tag`
	Digest   string   `json:"digest,omitempty"` // pinned digest, for a `digest`
}

type previewImagesRequest struct {
	EnvID      string `json:"env_id"`
	InlineYAML string `json:"inline_yaml"`
}

type previewImagesResponse struct {
	Images []previewImage `json:"images"`
}

// handlePreviewComposeImages parses the posted compose YAML and returns its unique images. It
// resolves nothing against a saved stack: it reads exactly what is in the editor, so a typo is
// answered here rather than at deploy time. Interpolated tags (`app:${TAG}`) come back as such —
// they can be validated but not rewritten, since a rewrite would clobber the variable.
func (s *Server) handlePreviewComposeImages(w http.ResponseWriter, r *http.Request) {
	var req previewImagesRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if !s.mayUseEnv(w, r, caps.StacksView, req.EnvID) {
		return
	}

	// Parse with no interpolation env: an unset ${VAR} resolves to empty and the ref fails to
	// parse, which is exactly the "interpolated" classification we want to surface.
	services, err := stacks.Parse(r.Context(), req.InlineYAML, "preview", nil)
	if err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	idx := map[string]int{}
	out := make([]previewImage, 0, len(services))
	for _, svc := range services {
		img := strings.TrimSpace(svc.Image)
		if img == "" {
			continue // build-only service, or an image that is entirely a variable
		}
		if i, ok := idx[img]; ok {
			out[i].Services = append(out[i].Services, svc.Name)
			continue
		}
		idx[img] = len(out)
		out = append(out, classifyImage(img, svc.Name))
	}

	httpx.JSON(w, http.StatusOK, previewImagesResponse{Images: out})
}

func classifyImage(image, service string) previewImage {
	pi := previewImage{Ref: image, Services: []string{service}}
	ref, err := dockerx.ParseImageRef(image)
	switch {
	case err != nil:
		pi.Kind = "interpolated" // an unresolved ${VAR}, or otherwise not a ref we can act on
	case ref.Digest != "":
		pi.Kind, pi.Host, pi.Repo, pi.Digest = "digest", ref.Host, ref.Repo, ref.Digest
	default:
		pi.Kind, pi.Host, pi.Repo, pi.Tag = "tag", ref.Host, ref.Repo, ref.Tag
	}
	return pi
}

type tagCheckRequest struct {
	EnvID string `json:"env_id"`
	Image string `json:"image"` // the image ref; its own tag is ignored, Tag is what we check
	Tag   string `json:"tag"`
}

type tagCheckResponse struct {
	Exists bool `json:"exists"`
	// Error is a registry-side reason (unreachable, unauthorized) for the field to show. It is
	// not an API error — one image failing must not fail the others.
	Error string `json:"error,omitempty"`
}

// handleCheckImageTag answers the one reliable question the upgrade flow needs: does this exact
// tag exist in its registry? One manifest read, with the stored credential for the image's host
// (anonymous if there is none).
func (s *Server) handleCheckImageTag(w http.ResponseWriter, r *http.Request) {
	var req tagCheckRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if !s.mayUseEnv(w, r, caps.StacksView, req.EnvID) {
		return
	}
	req.Tag = strings.TrimSpace(req.Tag)
	if req.Tag == "" {
		httpx.BadRequest(w, r, "A tag is required.")
		return
	}
	ref, err := dockerx.ParseImageRef(req.Image)
	if err != nil {
		httpx.BadRequest(w, r, "That image reference could not be parsed.")
		return
	}

	username, password, err := s.registryAuthForHost(r.Context(), ref.Host)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	exists, err := dockerx.TagExists(r.Context(), ref.Host, ref.Repo, req.Tag, username, password)
	resp := tagCheckResponse{Exists: exists}
	if err != nil {
		resp.Error = err.Error()
	}
	httpx.JSON(w, http.StatusOK, resp)
}

type latestHintRequest struct {
	EnvID string `json:"env_id"`
	Image string `json:"image"` // includes the current tag, which the newest is compared against
}

type latestHintResponse struct {
	// Latest is the newest tag that shares the current tag's shape, or "" for none. There is no
	// error field on purpose: the hint is a convenience, and any failure is simply no hint.
	Latest string `json:"latest,omitempty"`
}

// handleLatestImageTag returns a best-effort "the newest looks like…" hint. It never fails the
// request: an unparseable ref, an unreachable registry, or an unrankable tag list all come back
// as no hint. The heavy work (a tag list) is cached per repo in dockerx.
func (s *Server) handleLatestImageTag(w http.ResponseWriter, r *http.Request) {
	var req latestHintRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if !s.mayUseEnv(w, r, caps.StacksView, req.EnvID) {
		return
	}

	ref, err := dockerx.ParseImageRef(req.Image)
	if err != nil {
		httpx.JSON(w, http.StatusOK, latestHintResponse{})
		return
	}
	username, password, err := s.registryAuthForHost(r.Context(), ref.Host)
	if err != nil {
		httpx.JSON(w, http.StatusOK, latestHintResponse{})
		return
	}
	latest, _ := dockerx.LatestHint(r.Context(), ref.Host, ref.Repo, ref.Tag, username, password)
	httpx.JSON(w, http.StatusOK, latestHintResponse{Latest: latest})
}

type rewriteChange struct {
	OldRef string `json:"old_ref"` // the image string exactly as it appears in the YAML
	NewTag string `json:"new_tag"`
}

type rewriteRequest struct {
	EnvID      string          `json:"env_id"`
	InlineYAML string          `json:"inline_yaml"`
	Changes    []rewriteChange `json:"changes"`
}

type rewriteResponse struct {
	InlineYAML string `json:"inline_yaml"`
}

// handleRewriteComposeImages returns the YAML with the requested tags swapped in. It does not
// persist — the editor holds the result and the operator saves as usual — but rewriting the
// stack's definition IS an edit, so it takes StacksEdit. Digest-pinned and unparseable refs in
// the change set are skipped rather than erroring: the client should not send them, and one bad
// entry must not lose the rest.
func (s *Server) handleRewriteComposeImages(w http.ResponseWriter, r *http.Request) {
	var req rewriteRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if !s.mayUseEnv(w, r, caps.StacksEdit, req.EnvID) {
		return
	}

	changes := make(map[string]string, len(req.Changes))
	for _, c := range req.Changes {
		tag := strings.TrimSpace(c.NewTag)
		if tag == "" {
			continue
		}
		newRef, err := dockerx.SwapTag(c.OldRef, tag)
		if err != nil {
			continue
		}
		changes[c.OldRef] = newRef
	}

	out, err := stacks.RewriteImageTags(req.InlineYAML, changes)
	if err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, rewriteResponse{InlineYAML: out})
}

// registryAuthForHost finds the stored registry credential whose URL matches an image's host and
// returns its plaintext (the server already unseals these to build a deploy's pull auth — see
// deploy.go). No match means anonymous, which is correct for public images.
func (s *Server) registryAuthForHost(ctx context.Context, host string) (username, password string, err error) {
	regs, err := s.store.ListRegistries(ctx)
	if err != nil {
		return "", "", err
	}
	want := dockerx.RegistryHost(host)
	for _, reg := range regs {
		if dockerx.RegistryHost(reg.URL) != want {
			continue
		}
		pw, err := s.sealer.Open(reg.PasswordEnc)
		if err != nil {
			return "", "", fmt.Errorf("could not decrypt the credential for %s", reg.Name)
		}
		return reg.Username, pw, nil
	}
	return "", "", nil
}
