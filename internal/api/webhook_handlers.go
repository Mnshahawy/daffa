package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The webhook endpoint is the only route in Daffa that a machine on the OTHER side of a
// firewall is meant to call, and the only one that can start a deploy without a session.
// It therefore lives outside /api/ (so a browser cookie can never reach it) and is
// authenticated by an HMAC over the exact request body.
//
// A webhook is an object of its own, linked to any number of stacks and volume sources:
// one hook for a repository that feeds five stacks, or one organisation-level hook for
// every repository. What a push does to each target — repo, branch, paths, paused — is
// decided per target. See .ai/webhooks.md.
//
// The webhook id in the URL is not a secret and is not treated as one. The signature is
// the authentication.

const maxWebhookBody = 5 << 20 // push payloads carry commit lists; a few MB is plenty

// ── delivery ──────────────────────────────────────────────────────────────────────

func (s *Server) handleWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	wh, err := s.store.WebhookByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		// Do not confirm or deny which ids exist to an unauthenticated caller.
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "No such webhook.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.deliverWebhook(w, r, wh)
}

// handleLegacyStackWebhook serves /webhooks/stacks/{id}, the URL forges were configured
// with before webhooks were objects. It is an alias for the stack's webhook — the whole
// webhook, so it drives everything linked to it, not only this stack.
func (s *Server) handleLegacyStackWebhook(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.StackByID(r.Context(), r.PathValue("id"))
	if err != nil || st.WebhookID == "" {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			httpx.Error(w, r, err)
			return
		}
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "No such webhook.")
		return
	}
	s.deliverLinked(w, r, st.WebhookID)
}

func (s *Server) handleLegacyVolumeSourceWebhook(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.VolumeSourceByID(r.Context(), r.PathValue("id"))
	if err != nil || v.WebhookID == "" {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			httpx.Error(w, r, err)
			return
		}
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "No such webhook.")
		return
	}
	s.deliverLinked(w, r, v.WebhookID)
}

func (s *Server) deliverLinked(w http.ResponseWriter, r *http.Request, webhookID string) {
	wh, err := s.store.WebhookByID(r.Context(), webhookID)
	if err != nil {
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "No such webhook.")
		return
	}
	s.deliverWebhook(w, r, wh)
}

// deliveryResult is one target's verdict on one push. It is the response body, the
// stored last delivery, and what the settings page renders under "last delivery".
type deliveryResult struct {
	Kind   string `json:"kind"` // stack | volume_source
	ID     string `json:"id"`
	Name   string `json:"name"`
	EnvID  string `json:"env_id"`
	Status string `json:"status"` // deploying | syncing | ignored | covered | skipped | failed | deployed | synced
	Reason string `json:"reason,omitempty"`
	// DeploymentID is filled in once the deploy has been claimed, in the stored record only.
	DeploymentID string `json:"deployment_id,omitempty"`
}

// deliveryRecord is what last_delivery holds.
type deliveryRecord struct {
	Event   string           `json:"event"` // push | ping | test
	Ref     string           `json:"ref,omitempty"`
	Repos   []string         `json:"repos,omitempty"`
	Results []deliveryResult `json:"results"`
}

func (s *Server) deliverWebhook(w http.ResponseWriter, r *http.Request, wh *store.Webhook) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		httpx.BadRequest(w, r, "The payload could not be read.")
		return
	}

	// Verify BEFORE anything else is decided, so an unsigned caller learns nothing about
	// the configuration — not even what the webhook drives.
	secret, err := s.sealer.Open(wh.SecretEnc)
	if err != nil || secret == "" {
		s.auditWebhookDenied(r, wh, "the webhook secret could not be opened — was the master key replaced?")
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "No such webhook.")
		return
	}
	if err := verifySignature(r, body, secret); err != nil {
		s.auditWebhookDenied(r, wh, err.Error())
		httpx.Fail(w, r, http.StatusUnauthorized, "bad_signature",
			"The signature does not match. Check the secret configured on the git server.")
		return
	}

	// A ping is how every forge lets you test the hook. Answering it must not deploy — but
	// recording it is what lets the settings page say "the forge reached us".
	if isPing(r) {
		s.recordDelivery(context.WithoutCancel(r.Context()), wh.ID, deliveryRecord{Event: "ping", Results: []deliveryResult{}})
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}

	p, err := parsePush(body)
	if err != nil {
		httpx.BadRequest(w, r, "The payload is not a push event this understands.")
		return
	}

	// Forgejo's and Gitea's "Test delivery" is not a ping: it is a synthetic PUSH of the
	// head commit, with before == after and no file list. Taken at face value, "no file
	// list" means deploy everything (the act-rather-than-miss rule below), so testing a
	// hook redeployed every stack linked to it, all at once. A real push always moves the
	// branch — before == after is never one — so it is answered like the ping it means.
	if p.Test {
		s.recordDelivery(context.WithoutCancel(r.Context()), wh.ID, deliveryRecord{Event: "test", Ref: p.Ref, Repos: p.Repos, Results: []deliveryResult{}})
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "pong", "reason": "a test delivery — nothing was deployed"})
		return
	}

	linkedStacks, err := s.store.StacksByWebhook(r.Context(), wh.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	linkedSources, err := s.store.VolumeSourcesByWebhook(r.Context(), wh.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	results := planDelivery(p, linkedStacks, linkedSources)
	rec := deliveryRecord{Event: "push", Ref: p.Ref, Repos: p.Repos, Results: results}

	// Everything that acts runs in the background: a clone per stack is longer than a
	// forge's webhook timeout wants to wait, and the last-delivery record carries the
	// outcome either way. Detached from the request, which ends when we answer.
	acting := false
	for _, res := range results {
		if res.Status == "deploying" || res.Status == "syncing" {
			acting = true
		}
	}
	ip := s.clientIP(r)
	ctx := context.WithoutCancel(r.Context())
	s.recordDelivery(ctx, wh.ID, rec)
	if acting {
		// Its own copy of the verdicts: the goroutine rewrites them as targets finish, while
		// this handler is still serialising the originals into the response.
		bg := rec
		bg.Results = append([]deliveryResult(nil), results...)
		go s.runDelivery(ctx, wh.ID, ip, bg, linkedStacks, linkedSources)
	}

	status, code := "ignored", http.StatusOK
	if acting {
		status, code = "accepted", http.StatusAccepted
	}
	slog.Info("webhook delivery", "webhook", wh.Name, "ref", p.Ref, "targets", len(results), "acting", acting)
	httpx.JSON(w, code, map[string]any{"status": status, "results": results})
}

// planDelivery decides, per linked target, whether this push acts on it. Pure, so the rules
// are tested without a forge: paused, then repository, then branch, then paths — the order
// that gives the most useful reason when several would refuse.
func planDelivery(p *push, linkedStacks []*store.Stack, linkedSources []*store.VolumeSource) []deliveryResult {
	out := []deliveryResult{}
	deploying := map[string]string{} // stack id → name, for the covered rule below

	for _, st := range linkedStacks {
		res := deliveryResult{Kind: "stack", ID: st.ID, Name: st.Name, EnvID: st.EnvID}
		patterns := stacks.WatchPatterns(st.WatchPaths, st.GitPath)
		switch {
		case !st.AutoDeploy:
			res.Status, res.Reason = "ignored", "auto-deploy is paused on this stack"
		case st.SourceKind != "git":
			res.Status, res.Reason = "ignored", "this stack has no git source to push to"
		case !p.fromRepo(st.GitURL):
			res.Status, res.Reason = "ignored", "push was for "+strings.Join(p.Repos, ", ")+", this stack tracks "+repoKey(st.GitURL)
		// Wrong branch. A push to a feature branch must not deploy what `main` is pinned to.
		case p.Ref != "" && st.GitRef != "" && !refMatches(p.Ref, st.GitRef):
			res.Status, res.Reason = "ignored", "push was to "+p.Ref+", this stack tracks "+st.GitRef
		// If the payload carried no file list at all (some forges omit it on large pushes),
		// deploy rather than silently do nothing: a missed deploy is a worse failure than a
		// redundant one, and `compose up` on an unchanged stack is close to a no-op.
		case len(p.Files) > 0 && !stacks.Matches(p.Files, patterns):
			res.Status, res.Reason = "ignored", "no watched path was changed"
		default:
			res.Status = "deploying"
			deploying[st.ID] = st.Name
		}
		out = append(out, res)
	}

	for _, v := range linkedSources {
		res := deliveryResult{Kind: "volume_source", ID: v.ID, Name: v.Volume, EnvID: v.EnvID}
		switch {
		case !v.AutoSync:
			res.Status, res.Reason = "ignored", "auto-sync is paused on this source"
		case v.SourceKind != "git":
			res.Status, res.Reason = "ignored", "this source has no git repository to push to"
		case !p.fromRepo(v.GitURL):
			res.Status, res.Reason = "ignored", "push was for "+strings.Join(p.Repos, ", ")+", this source tracks "+repoKey(v.GitURL)
		case p.Ref != "" && v.GitRef != "" && !refMatches(p.Ref, v.GitRef):
			res.Status, res.Reason = "ignored", "push was to "+p.Ref+", this source tracks "+v.GitRef
		// An empty file list syncs rather than silently doing nothing; the hash makes a
		// redundant sync cost one clone.
		case len(p.Files) > 0 && !touchesSubtree(p.Files, v.GitPath):
			res.Status, res.Reason = "ignored", "no file under "+path.Clean("/"+v.GitPath)+" was changed"
		// Its stack deploys in this same delivery, and a deploy syncs its sources FIRST —
		// a second, concurrent sync of the same volume would only race it.
		case deploying[v.StackID] != "":
			res.Status, res.Reason = "covered", "synced by the deploy of "+deploying[v.StackID]
		default:
			res.Status = "syncing"
		}
		out = append(out, res)
	}
	return out
}

// runDelivery acts on the targets planDelivery accepted, in parallel — they are different
// stacks and volumes, and one slow clone should not hold the rest — then rewrites the last
// delivery with how each one went.
func (s *Server) runDelivery(ctx context.Context, webhookID, ip string, rec deliveryRecord,
	linkedStacks []*store.Stack, linkedSources []*store.VolumeSource) {
	byStack := map[string]*store.Stack{}
	for _, st := range linkedStacks {
		byStack[st.ID] = st
	}
	bySource := map[string]*store.VolumeSource{}
	for _, v := range linkedSources {
		bySource[v.ID] = v
	}

	var wg sync.WaitGroup
	for i := range rec.Results {
		res := &rec.Results[i]
		switch res.Status {
		case "deploying":
			st := byStack[res.ID]
			wg.Add(1)
			go func() {
				defer wg.Done()
				// No user: a push started this, and the deployment records that rather than
				// attributing it to whoever last touched the stack.
				dep, err := s.deploy(ctx, st, stacks.ActionUp, store.TriggerWebhook, "", nil)
				switch {
				case errors.Is(err, store.ErrRunInProgress):
					// The previous deploy is still going. Doing nothing is right: it will finish
					// with a bundle at least as new as this push, or the next push picks it up.
					res.Status, res.Reason = "skipped", "a deploy was already running for this stack"
					s.auditTargetWebhook(ctx, ip, "stack.webhook", st.EnvID, st.Name, "ok", res.Reason)
				case err != nil:
					res.Status, res.Reason = "failed", err.Error()
					s.auditTargetWebhook(ctx, ip, "stack.webhook", st.EnvID, st.Name, "error", err.Error())
				default:
					res.Status, res.DeploymentID = "deployed", dep.ID
					s.auditTargetWebhook(ctx, ip, "stack.webhook", st.EnvID, st.Name, "ok", "")
				}
			}()
		case "syncing":
			v := *bySource[res.ID]
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.reportVolumeSourceSync(ctx, &v); err != nil {
					res.Status, res.Reason = "failed", err.Error()
					s.auditTargetWebhook(ctx, ip, "volsource.webhook", v.EnvID, v.Volume, "error", err.Error())
					return
				}
				res.Status = "synced"
				s.auditTargetWebhook(ctx, ip, "volsource.webhook", v.EnvID, v.Volume, "ok", "")
			}()
		}
	}
	wg.Wait()
	s.recordDelivery(ctx, webhookID, rec)
}

func (s *Server) recordDelivery(ctx context.Context, webhookID string, rec deliveryRecord) {
	b, err := json.Marshal(rec)
	if err == nil {
		err = s.store.RecordWebhookDelivery(ctx, webhookID, string(b))
	}
	if err != nil {
		slog.Error("recording a webhook delivery", "webhook", webhookID, "err", err)
	}
}

// auditTargetWebhook records a delivery against the TARGET, on its cluster, so a
// cluster-scoped auditor sees the pushes that touched their own stacks.
func (s *Server) auditTargetWebhook(ctx context.Context, ip, action, envID, target, outcome, reason string) {
	detail := map[string]string{"ip": ip}
	if reason != "" {
		detail["reason"] = reason
	}
	s.audit(ctx, store.AuditEntry{
		EnvID: envID, Action: action, Target: target,
		Outcome: outcome, Detail: store.AuditDetail(detail),
	})
}

// auditWebhookDenied records a refused delivery against the webhook itself — it acted on
// nothing, so there is no target to file it under.
func (s *Server) auditWebhookDenied(r *http.Request, wh *store.Webhook, reason string) {
	s.audit(r.Context(), store.AuditEntry{
		EnvID: wh.EnvID, Action: "webhook.delivery", Target: wh.Name, Outcome: "denied",
		Detail: store.AuditDetail(map[string]string{"ip": s.clientIP(r), "reason": reason}),
	})
}

// ── signature ───────────────────────────────────────────────────────────────────

// verifySignature accepts the three shapes the forges actually send. All are an HMAC of
// the raw body under the shared secret, except GitLab's, which is the secret itself.
func verifySignature(r *http.Request, body []byte, secret string) error {
	// GitHub, and Forgejo/Gitea in recent versions.
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		return checkHMAC(strings.TrimPrefix(sig, "sha256="), body, secret)
	}
	// Forgejo and Gitea: the same HMAC, unprefixed.
	if sig := firstNonEmpty(r.Header.Get("X-Forgejo-Signature"), r.Header.Get("X-Gitea-Signature")); sig != "" {
		return checkHMAC(sig, body, secret)
	}
	// GitLab sends the secret verbatim. Weaker (it is replayable and does not cover the
	// body), but it is what GitLab does, and refusing it would just mean GitLab users
	// cannot use webhooks at all.
	if tok := r.Header.Get("X-Gitlab-Token"); tok != "" {
		if subtleCompare(tok, secret) {
			return nil
		}
		return errors.New("the GitLab token does not match")
	}

	return errors.New("the request carried no signature")
}

func checkHMAC(got string, body []byte, secret string) error {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	// Constant-time: a byte-by-byte comparison would leak the expected signature to
	// anyone patient enough to time it.
	if !subtleCompare(strings.TrimSpace(got), want) {
		return errors.New("the signature does not match the payload")
	}
	return nil
}

func subtleCompare(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

func isPing(r *http.Request) bool {
	event := firstNonEmpty(
		r.Header.Get("X-GitHub-Event"),
		r.Header.Get("X-Forgejo-Event"),
		r.Header.Get("X-Gitea-Event"),
		r.Header.Get("X-Gitlab-Event"),
	)
	return strings.EqualFold(event, "ping")
}

// ── payload ─────────────────────────────────────────────────────────────────────

type push struct {
	Ref string
	// Test is a forge's "test delivery" dressed as a push (before == after).
	Test  bool
	Files []string
	// Repos is every repository identity the payload names, as repoKey forms. A forge
	// sends several URLs for one repository (clone, ssh, web); a target matches any.
	Repos []string
}

// fromRepo reports whether the push is for the repository at gitURL. A payload that names
// no repository at all is not filtered — the same "act rather than miss" rule as an empty
// file list — which keeps a forge that sends bare payloads working.
func (p *push) fromRepo(gitURL string) bool {
	want := repoKey(gitURL)
	// A URL repoKey cannot read (a local path) cannot be compared either; filtering on it
	// would silently ignore every push.
	if len(p.Repos) == 0 || want == "" {
		return true
	}
	for _, r := range p.Repos {
		if r == want {
			return true
		}
	}
	return false
}

// parsePush pulls out what the per-target rules need: which branch, which files, which
// repository. GitHub, Forgejo, Gitea and GitLab share the shape for the first two; the
// repository is spread over several fields that differ by forge, so all are collected.
func parsePush(body []byte) (*push, error) {
	type repoURLs struct {
		CloneURL   string `json:"clone_url"`
		SSHURL     string `json:"ssh_url"`
		GitURL     string `json:"git_url"`
		HTMLURL    string `json:"html_url"`
		URL        string `json:"url"`
		GitHTTPURL string `json:"git_http_url"` // GitLab
		GitSSHURL  string `json:"git_ssh_url"`  // GitLab
		WebURL     string `json:"web_url"`      // GitLab project
		Homepage   string `json:"homepage"`     // GitLab repository
	}
	var payload struct {
		Ref        string   `json:"ref"`
		Before     string   `json:"before"`
		After      string   `json:"after"`
		Repository repoURLs `json:"repository"`
		Project    repoURLs `json:"project"`
		Commits    []struct {
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	p := &push{Ref: payload.Ref, Test: payload.Before != "" && payload.Before == payload.After}
	seen := map[string]bool{}
	for _, c := range payload.Commits {
		for _, list := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range list {
				if !seen[f] {
					seen[f] = true
					p.Files = append(p.Files, f)
				}
			}
		}
	}

	repos := map[string]bool{}
	for _, r := range []repoURLs{payload.Repository, payload.Project} {
		for _, u := range []string{r.CloneURL, r.SSHURL, r.GitURL, r.HTMLURL, r.URL, r.GitHTTPURL, r.GitSSHURL, r.WebURL, r.Homepage} {
			// An API URL identifies nothing a stack would be configured with, so it is
			// skipped rather than normalised wrongly: GitHub's repository.url is
			// api.github.com/repos/… outside push events, and Forgejo's and Gitea's is
			// always <host>/api/v1/repos/….
			if k := repoKey(u); k != "" && !isAPIRepoKey(k) && !repos[k] {
				repos[k] = true
				p.Repos = append(p.Repos, k)
			}
		}
	}
	return p, nil
}

func isAPIRepoKey(k string) bool {
	if strings.HasPrefix(k, "api.") {
		return true
	}
	_, path, _ := strings.Cut(k, "/")
	return strings.HasPrefix(path, "api/v1/repos/") || strings.HasPrefix(path, "api/v4/projects/")
}

// repoKey reduces a repository URL to host/path — lowercase, without scheme, user, port,
// trailing slash or .git — so the forms one forge sends for one repository compare equal:
//
//	https://github.com/Acme/App.git   git@github.com:acme/app   ssh://git@github.com:22/acme/app
//
// all become github.com/acme/app. The port is dropped because a self-hosted forge serves
// ssh and https on different ones for the same repository.
func repoKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var host, p string
	if !strings.Contains(raw, "://") {
		// scp-like: [user@]host:path
		at := strings.LastIndex(raw, "@")
		rest := raw[at+1:]
		i := strings.Index(rest, ":")
		if i < 0 {
			return ""
		}
		host, p = rest[:i], rest[i+1:]
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ""
		}
		host, p = u.Hostname(), u.Path
	}
	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".git")
	if host == "" || p == "" {
		return ""
	}
	return strings.ToLower(host + "/" + p)
}

// refMatches compares a pushed ref against what a target tracks, tolerating the fact that
// a stack is configured with "main" while a push says "refs/heads/main".
func refMatches(pushed, tracked string) bool {
	norm := func(s string) string {
		s = strings.TrimPrefix(s, "refs/heads/")
		s = strings.TrimPrefix(s, "refs/tags/")
		return s
	}
	return norm(pushed) == norm(tracked)
}

// ── managing webhooks ─────────────────────────────────────────────────────────────

func withWebhook(ctx context.Context, wh *store.Webhook) context.Context {
	return context.WithValue(ctx, webhookKey, wh)
}

func webhookFrom(ctx context.Context) (*store.Webhook, bool) {
	wh, ok := ctx.Value(webhookKey).(*store.Webhook)
	return wh, ok
}

// requireOnWebhook resolves the {id} webhook and checks the capability where it lives: on
// its cluster, or — for a fleet-wide one — globally, the monitor rule. 404 rather than
// 403, the stack precedent.
func (s *Server) requireOnWebhook(c caps.Cap) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := auth.UserFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}
			wh, err := s.store.WebhookByID(r.Context(), r.PathValue("id"))
			if errors.Is(err, store.ErrNotFound) {
				httpx.Fail(w, r, http.StatusNotFound, "no_such_webhook", "No such webhook.")
				return
			}
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			allowed := u.Caps.Has(c, wh.EnvID)
			if wh.EnvID == "" {
				allowed = u.Caps.Global.Has(c)
			}
			if !allowed {
				s.recordDenial(r, u, "missing_capability:"+c.Name())
				httpx.Fail(w, r, http.StatusNotFound, "no_such_webhook", "No such webhook.")
				return
			}
			next.ServeHTTP(w, r.WithContext(withWebhook(r.Context(), wh)))
		})
	}
}

type webhookTargetView struct {
	Kind    string `json:"kind"` // stack | volume_source
	ID      string `json:"id"`
	Name    string `json:"name"`
	EnvID   string `json:"env_id"`
	EnvName string `json:"env_name,omitempty"`
	Paused  bool   `json:"paused"`
}

type webhookView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	EnvID   string `json:"env_id,omitempty"` // absent = fleet-wide
	EnvName string `json:"env_name,omitempty"`
	// Path is where the forge posts, relative to Daffa's public URL — the browser knows the
	// origin it is on, the server may not.
	Path    string              `json:"path"`
	Targets []webhookTargetView `json:"targets"`
	// HiddenTargets counts what this webhook drives on clusters the caller cannot see: they
	// learn it is in use (and why it cannot be deleted) without learning what by.
	HiddenTargets  int             `json:"hidden_targets"`
	LastDeliveryAt *time.Time      `json:"last_delivery_at,omitempty"`
	LastDelivery   *deliveryRecord `json:"last_delivery,omitempty"`
}

func (s *Server) viewWebhook(r *http.Request, wh *store.Webhook) (webhookView, error) {
	ctx := r.Context()
	v := webhookView{ID: wh.ID, Name: wh.Name, EnvID: wh.EnvID, Path: "/webhooks/" + wh.ID, Targets: []webhookTargetView{}}
	if wh.EnvID != "" {
		v.EnvName = s.envName(ctx, wh.EnvID)
	}
	u, _ := auth.UserFrom(ctx)
	sees := func(c caps.Cap, envID string) bool { return u != nil && u.Caps.Has(c, envID) }

	linkedStacks, err := s.store.StacksByWebhook(ctx, wh.ID)
	if err != nil {
		return v, err
	}
	for _, st := range linkedStacks {
		if !sees(caps.StacksView, st.EnvID) {
			v.HiddenTargets++
			continue
		}
		v.Targets = append(v.Targets, webhookTargetView{Kind: "stack", ID: st.ID, Name: st.Name,
			EnvID: st.EnvID, EnvName: s.envName(ctx, st.EnvID), Paused: !st.AutoDeploy})
	}
	linkedSources, err := s.store.VolumeSourcesByWebhook(ctx, wh.ID)
	if err != nil {
		return v, err
	}
	for _, src := range linkedSources {
		if !sees(caps.VolSourcesView, src.EnvID) {
			v.HiddenTargets++
			continue
		}
		v.Targets = append(v.Targets, webhookTargetView{Kind: "volume_source", ID: src.ID, Name: src.Volume,
			EnvID: src.EnvID, EnvName: s.envName(ctx, src.EnvID), Paused: !src.AutoSync})
	}

	if !wh.LastDeliveryAt.IsZero() {
		t := wh.LastDeliveryAt
		v.LastDeliveryAt = &t
	}
	var rec deliveryRecord
	if wh.LastDelivery != "" && json.Unmarshal([]byte(wh.LastDelivery), &rec) == nil {
		// The same visibility rule as the targets: a verdict names a stack.
		kept := rec.Results[:0]
		for _, res := range rec.Results {
			c := caps.StacksView
			if res.Kind == "volume_source" {
				c = caps.VolSourcesView
			}
			if sees(c, res.EnvID) {
				kept = append(kept, res)
			}
		}
		rec.Results = kept
		v.LastDelivery = &rec
	}
	return v, nil
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.WebhooksView)
	list, err := s.store.ListWebhooks(r.Context(), global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]webhookView, 0, len(list))
	for _, wh := range list {
		v, err := s.viewWebhook(r, wh)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type webhookRequest struct {
	Name string `json:"name"`
	// EnvID pins the webhook to one cluster; empty makes it fleet-wide, which needs
	// webhooks.edit globally. Immutable once created.
	EnvID string `json:"env_id,omitempty"`
}

// webhookSecretResponse carries the secret the one time it exists in plaintext: at
// creation and at rotation. It is sealed afterwards and never handed back.
type webhookSecretResponse struct {
	Webhook webhookView `json:"webhook"`
	Secret  string      `json:"secret"`
}

func validWebhookName(n string) bool {
	return n != "" && len(n) <= 128 && strings.TrimSpace(n) == n && !strings.ContainsAny(n, "\r\n")
}

// mintWebhookSecret returns a fresh secret and its sealed form.
func (s *Server) mintWebhookSecret() (plain, sealed string, err error) {
	if plain, err = randomToken(); err != nil {
		return "", "", err
	}
	sealed, err = s.sealer.Seal(plain)
	return plain, sealed, err
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req webhookRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	// Body-scoped: the cluster is in the payload. Fleet-wide needs the grant fleet-wide.
	if req.EnvID == "" {
		if u, ok := auth.UserFrom(r.Context()); !ok || !u.Caps.Global.Has(caps.WebhooksEdit) {
			httpx.Fail(w, r, http.StatusForbidden, "missing_capability:"+caps.WebhooksEdit.Name(),
				"A fleet-wide webhook needs webhooks.edit on every cluster. Pin it to a cluster you manage instead.")
			return
		}
	} else {
		if !s.mayUseEnv(w, r, caps.WebhooksEdit, req.EnvID) {
			return
		}
		if _, err := s.store.EnvironmentByID(r.Context(), req.EnvID); err != nil {
			httpx.BadRequest(w, r, "No such cluster.")
			return
		}
	}
	req.Name = strings.TrimSpace(req.Name)
	if !validWebhookName(req.Name) {
		httpx.BadRequest(w, r, "A webhook needs a name — one line, at most 128 characters.")
		return
	}
	wh, secret, ok := s.createWebhook(w, r, req.Name, req.EnvID)
	if !ok {
		return
	}
	v, err := s.viewWebhook(r, wh)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, webhookSecretResponse{Webhook: v, Secret: secret})
}

// createWebhook mints, stores and audits a webhook, answering the request itself on
// failure. Shared by the settings page's create and the stack panel's "create one for this
// stack", so both refuse a duplicate name the same way.
func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request, name, envID string) (*store.Webhook, string, bool) {
	secret, sealed, err := s.mintWebhookSecret()
	if err != nil {
		httpx.Error(w, r, err)
		return nil, "", false
	}
	wh := &store.Webhook{Name: name, EnvID: envID, SecretEnc: sealed}
	if u, ok := auth.UserFrom(r.Context()); ok {
		wh.CreatedBy = u.ID
	}
	if err := s.store.CreateWebhook(r.Context(), wh); err != nil {
		if errors.Is(err, store.ErrWebhookNameTaken) {
			httpx.Fail(w, r, http.StatusBadRequest, "webhook_name_taken",
				"A webhook called "+name+" already exists. Link that one, or pick another name.")
			return nil, "", false
		}
		httpx.Error(w, r, err)
		return nil, "", false
	}
	s.audit(r.Context(), store.AuditEntry{EnvID: envID, Action: "webhook.create", Target: name, Outcome: "ok"})
	return wh, secret, true
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	wh, _ := webhookFrom(r.Context())
	var req webhookRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if req.EnvID != "" && req.EnvID != wh.EnvID {
		httpx.BadRequest(w, r, "A webhook's cluster cannot change — create a new one and relink its targets.")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !validWebhookName(req.Name) {
		httpx.BadRequest(w, r, "A webhook needs a name — one line, at most 128 characters.")
		return
	}
	if err := s.store.RenameWebhook(r.Context(), wh.ID, req.Name); err != nil {
		if errors.Is(err, store.ErrWebhookNameTaken) {
			httpx.Fail(w, r, http.StatusBadRequest, "webhook_name_taken", "A webhook called "+req.Name+" already exists.")
			return
		}
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{EnvID: wh.EnvID, Action: "webhook.rename", Target: req.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"from": wh.Name})})
	wh.Name = req.Name
	v, err := s.viewWebhook(r, wh)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// handleRotateWebhook mints a fresh secret. The old one stops verifying immediately, for
// every target — which is the point when it has leaked, and the warning in the UI when it
// has not.
func (s *Server) handleRotateWebhook(w http.ResponseWriter, r *http.Request) {
	wh, _ := webhookFrom(r.Context())
	secret, sealed, err := s.mintWebhookSecret()
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.store.SetWebhookSecret(r.Context(), wh.ID, sealed); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{EnvID: wh.EnvID, Action: "webhook.rotate", Target: wh.Name, Outcome: "ok"})
	v, err := s.viewWebhook(r, wh)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, webhookSecretResponse{Webhook: v, Secret: secret})
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	wh, _ := webhookFrom(r.Context())
	n, err := s.store.WebhookInUse(r.Context(), wh.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// Refuse, don't orphan: an auto-deploy whose webhook vanished would fail silently at
	// the next push, which is the failure nobody notices until it matters.
	if n > 0 {
		httpx.Fail(w, r, http.StatusConflict, "webhook_in_use",
			fmt.Sprintf("This webhook still drives %d stack(s) or volume source(s). Unlink them first.", n))
		return
	}
	if err := s.store.DeleteWebhook(r.Context(), wh.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{EnvID: wh.EnvID, Action: "webhook.delete", Target: wh.Name, Outcome: "ok"})
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// freeWebhookName picks a name for a webhook made on a target's behalf: the target's own,
// or suffixed with its cluster when another cluster's "api" got there first — the same
// scheme migration 0021 used, so the names read the same however they were made.
func (s *Server) freeWebhookName(ctx context.Context, base, cluster string) (string, error) {
	for _, n := range []string{base, base + "-" + cluster} {
		_, err := s.store.WebhookByName(ctx, n)
		if errors.Is(err, store.ErrNotFound) {
			return n, nil
		}
		if err != nil {
			return "", err
		}
	}
	return base + "-" + strings.ToLower(store.NewID()[:6]), nil
}

// linkableWebhook resolves a webhook a target on envID may link to: it must exist, and be
// fleet-wide or on that same cluster — a push through a staging-only webhook must not be
// able to deploy prod. "" is "no webhook" and always allowed.
func (s *Server) linkableWebhook(ctx context.Context, webhookID, envID string) (string, error) {
	if webhookID == "" {
		return "", nil
	}
	wh, err := s.store.WebhookByID(ctx, webhookID)
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("No such webhook.")
	}
	if err != nil {
		return "", err
	}
	if wh.EnvID != "" && wh.EnvID != envID {
		return "", errors.New("That webhook belongs to another cluster. Link a fleet-wide webhook, or one on this cluster.")
	}
	return wh.ID, nil
}

// ── a stack's auto-deploy ───────────────────────────────────────────────────────

type autoDeployRequest struct {
	Enabled    bool   `json:"enabled"`
	WatchPaths string `json:"watch_paths"`
	// WebhookID links the webhook whose pushes deploy this stack; "" unlinks.
	WebhookID string `json:"webhook_id"`
	// CreateWebhook, instead of WebhookID, makes a webhook on this stack's cluster named after
	// the stack and links it — the one-step path the panel offers. Its secret comes back once.
	CreateWebhook bool `json:"create_webhook,omitempty"`
}

type autoDeployResponse struct {
	Enabled   bool   `json:"enabled"`
	WebhookID string `json:"webhook_id,omitempty"`
	// Secret appears only when CreateWebhook minted one.
	Secret string `json:"secret,omitempty"`
	// Watch is the resolved list of patterns a push must touch, including the default.
	Watch []string `json:"watch"`
}

// handleSetAutoDeploy links a stack to a webhook (or unlinks it), pauses or resumes it,
// and sets what it watches.
func (s *Server) handleSetAutoDeploy(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}

	var req autoDeployRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	if (req.Enabled || req.WebhookID != "" || req.CreateWebhook) && stack.SourceKind != "git" {
		httpx.BadRequest(w, r, "Auto-deploy needs a git source — an inline compose file has nothing to push to.")
		return
	}

	resp := autoDeployResponse{Enabled: req.Enabled}
	webhookID := req.WebhookID
	switch {
	case req.CreateWebhook:
		// Creating is its own power: webhooks.edit on this cluster, checked here because
		// the route is scoped to the stack, not to webhooks.
		if !s.mayUseEnv(w, r, caps.WebhooksEdit, stack.EnvID) {
			return
		}
		name, err := s.freeWebhookName(r.Context(), stack.Name, s.envName(r.Context(), stack.EnvID))
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		wh, secret, ok := s.createWebhook(w, r, name, stack.EnvID)
		if !ok {
			return
		}
		webhookID, resp.Secret = wh.ID, secret
	default:
		id, err := s.linkableWebhook(r.Context(), webhookID, stack.EnvID)
		if err != nil {
			httpx.BadRequest(w, r, err.Error())
			return
		}
		webhookID = id
	}
	if req.Enabled && webhookID == "" {
		httpx.BadRequest(w, r, "Pick the webhook whose pushes should deploy this stack, or create one for it.")
		return
	}

	if err := s.store.SetStackAutoDeploy(r.Context(), stack.ID, req.Enabled, webhookID, strings.TrimSpace(req.WatchPaths)); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: stack.EnvID, Action: "stack.autodeploy", Target: stack.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{
			"enabled": req.Enabled, "webhook": webhookID, "watch": req.WatchPaths,
		}),
	})

	resp.WebhookID = webhookID
	resp.Watch = stacks.WatchPatterns(strings.TrimSpace(req.WatchPaths), stack.GitPath)
	httpx.JSON(w, http.StatusOK, resp)
}
