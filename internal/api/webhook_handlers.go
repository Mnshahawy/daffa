package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The webhook endpoint is the only route in Daffa that a machine on the OTHER side of a
// firewall is meant to call, and the only one that can start a deploy without a session.
// It therefore lives outside /api/ (so a browser cookie can never reach it) and is
// authenticated by an HMAC over the exact request body, with a secret that exists only
// for this one stack.
//
// The stack id in the URL is not a secret and is not treated as one. The signature is the
// authentication.

const maxWebhookBody = 5 << 20 // push payloads carry commit lists; a few MB is plenty

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	stack, err := s.store.StackByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		// Do not confirm or deny which stack ids exist to an unauthenticated caller.
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "No such webhook.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		httpx.BadRequest(w, r, "The payload could not be read.")
		return
	}

	// Verify BEFORE anything else is decided, so an unsigned caller learns nothing about
	// the stack's configuration — not even whether auto-deploy is on.
	secret, err := s.sealer.Open(stack.WebhookSecretEnc)
	if err != nil || secret == "" {
		s.auditWebhook(r, stack, "denied", "auto-deploy is not enabled for this stack")
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "No such webhook.")
		return
	}
	if err := verifySignature(r, body, secret); err != nil {
		s.auditWebhook(r, stack, "denied", err.Error())
		httpx.Fail(w, r, http.StatusUnauthorized, "bad_signature",
			"The signature does not match. Check the secret configured on the git server.")
		return
	}

	// A ping is how every forge lets you test the hook. Answering it must not deploy.
	if isPing(r) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}

	if !stack.AutoDeploy {
		httpx.JSON(w, http.StatusOK, map[string]string{
			"status": "ignored", "reason": "auto-deploy is turned off for this stack",
		})
		return
	}

	push, err := parsePush(body)
	if err != nil {
		httpx.BadRequest(w, r, "The payload is not a push event this understands.")
		return
	}

	// Wrong branch. A push to a feature branch must not deploy what `main` is pinned to.
	if push.Ref != "" && stack.GitRef != "" && !refMatches(push.Ref, stack.GitRef) {
		httpx.JSON(w, http.StatusOK, map[string]string{
			"status": "ignored", "reason": "push was to " + push.Ref + ", this stack tracks " + stack.GitRef,
		})
		return
	}

	// Nothing this stack watches was touched.
	patterns := stacks.WatchPatterns(stack.WatchPaths, stack.GitPath)
	if len(push.Files) > 0 && !stacks.Matches(push.Files, patterns) {
		httpx.JSON(w, http.StatusOK, map[string]string{
			"status": "ignored", "reason": "no watched path was changed",
		})
		return
	}
	// If the payload carried no file list at all (some forges omit it on large pushes),
	// we deploy rather than silently do nothing: a missed deploy is a worse failure than
	// a redundant one, and `compose up` on an unchanged stack is close to a no-op.

	// No user: a push started this, and the deployment records that rather than attributing it
	// to whoever last touched the stack.
	dep, err := s.deploy(r.Context(), stack, stacks.ActionUp, store.TriggerWebhook, "", nil)
	switch {
	case errors.Is(err, store.ErrRunInProgress):
		// The previous deploy is still going. Doing nothing is right: it will finish with
		// a bundle at least as new as this push, or the next push will pick it up.
		httpx.JSON(w, http.StatusConflict, map[string]string{
			"status": "skipped", "reason": "a deploy is already running for this stack",
		})
		return
	case err != nil:
		s.auditWebhook(r, stack, "error", err.Error())
		httpx.Fail(w, r, http.StatusBadGateway, "deploy_failed", err.Error())
		return
	}

	s.auditWebhook(r, stack, "ok", "")
	slog.Info("webhook deploy started", "stack", stack.Name, "ref", push.Ref, "deployment", dep.ID)

	httpx.JSON(w, http.StatusAccepted, map[string]string{
		"status": "deploying", "deployment_id": dep.ID,
	})
}

func (s *Server) auditWebhook(r *http.Request, stack *store.Stack, outcome, reason string) {
	detail := map[string]string{"ip": s.clientIP(r)}
	if reason != "" {
		detail["reason"] = reason
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: stack.EnvID, Action: "stack.webhook", Target: stack.Name,
		Outcome: outcome, Detail: store.AuditDetail(detail),
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
	Ref   string
	Files []string
}

// parsePush pulls out the only two things that matter: which branch, and which files.
// GitHub, Forgejo, Gitea and GitLab all use the same shape for these.
func parsePush(body []byte) (*push, error) {
	var payload struct {
		Ref     string `json:"ref"`
		Commits []struct {
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	p := &push{Ref: payload.Ref}
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
	return p, nil
}

// refMatches compares a pushed ref against what the stack tracks, tolerating the fact
// that a stack is configured with "main" while a push says "refs/heads/main".
func refMatches(pushed, tracked string) bool {
	norm := func(s string) string {
		s = strings.TrimPrefix(s, "refs/heads/")
		s = strings.TrimPrefix(s, "refs/tags/")
		return s
	}
	return norm(pushed) == norm(tracked)
}

// ── enabling it ─────────────────────────────────────────────────────────────────

type autoDeployRequest struct {
	Enabled    bool   `json:"enabled"`
	WatchPaths string `json:"watch_paths"`
	// Rotate mints a fresh secret. The old one stops working immediately.
	Rotate bool `json:"rotate"`
}

type autoDeployResponse struct {
	Enabled bool `json:"enabled"`
	// Secret appears exactly once, when it is minted (first enable, or a rotation). It is
	// sealed in the database afterwards and never handed back.
	Secret string `json:"secret,omitempty"`
	// Watch is the resolved list of patterns a push must touch, including the default.
	Watch []string `json:"watch"`
}

// handleSetAutoDeploy turns auto-deploy on or off and returns the webhook secret when
// there is a new one to show. The secret is displayed once, here — it lives sealed in the
// database afterwards and is never handed back.
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

	if req.Enabled && stack.SourceKind != "git" {
		httpx.BadRequest(w, r, "Auto-deploy needs a git source — an inline compose file has nothing to push to.")
		return
	}

	resp := autoDeployResponse{Enabled: req.Enabled}

	// Mint a secret when enabling for the first time, or when asked to rotate.
	if req.Enabled && (stack.WebhookSecretEnc == "" || req.Rotate) {
		secret, err := randomToken()
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		sealed, err := s.sealer.Seal(secret)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if err := s.store.SetStackWebhookSecret(r.Context(), stack.ID, sealed); err != nil {
			httpx.Error(w, r, err)
			return
		}
		resp.Secret = secret // shown once
	}

	if err := s.store.SetStackAutoDeploy(r.Context(), stack.ID, req.Enabled, strings.TrimSpace(req.WatchPaths)); err != nil {
		httpx.Error(w, r, err)
		return
	}

	var userID string
	if u, ok := auth.UserFrom(r.Context()); ok {
		userID = u.ID
	}
	s.audit(r.Context(), store.AuditEntry{
		UserID: userID, EnvID: stack.EnvID, Action: "stack.autodeploy", Target: stack.Name,
		Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{
			"enabled": req.Enabled, "rotated": req.Rotate, "watch": req.WatchPaths,
		}),
	})

	resp.Watch = stacks.WatchPatterns(strings.TrimSpace(req.WatchPaths), stack.GitPath)
	httpx.JSON(w, http.StatusOK, resp)
}
