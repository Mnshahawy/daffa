package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// dockerVolumeName is Docker's own grammar for a volume name, enforced server-side
// everywhere one is accepted — the difference between this and a client-side-only check
// is precisely the API caller who never loads the form.
var dockerVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type volSourceFileView struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    int64  `json:"mode,omitempty"`
}

type volumeSourceView struct {
	ID              string     `json:"id"`
	EnvID           string     `json:"env_id"`
	EnvName         string     `json:"env_name,omitempty"`
	Volume          string     `json:"volume"`
	SourceKind      string     `json:"source_kind"` // git | inline
	GitURL          string     `json:"git_url,omitempty"`
	GitRef          string     `json:"git_ref,omitempty"`
	GitPath         string     `json:"git_path,omitempty"`
	GitCredentialID string     `json:"git_credential_id,omitempty"` // the credential's id; never its secret
	UID             int        `json:"uid"`
	GID             int        `json:"gid"`
	StackID         string     `json:"stack_id,omitempty"`
	StackName       string     `json:"stack_name,omitempty"`
	RestartTargets  string     `json:"restart_targets,omitempty"`
	AutoSync        bool       `json:"auto_sync"`
	HasWebhook      bool       `json:"has_webhook_secret"`
	SyncedCommit    string     `json:"synced_commit,omitempty"`
	SyncedAt        *time.Time `json:"synced_at,omitempty"`
	Status          string     `json:"status"`
	LastError       string     `json:"last_error,omitempty"`
	Warnings        []string   `json:"warnings,omitempty"`
	// Files is populated only on the single-source GET, for an inline source — the list
	// view omits it (contents can be large, and the list does not need them).
	Files []volSourceFileView `json:"files,omitempty"`
}

func (s *Server) viewVolumeSource(ctx context.Context, v *store.VolumeSource) volumeSourceView {
	out := volumeSourceView{
		ID: v.ID, EnvID: v.EnvID, EnvName: s.envName(ctx, v.EnvID),
		Volume: v.Volume, SourceKind: v.SourceKind, GitURL: v.GitURL, GitRef: v.GitRef, GitPath: v.GitPath,
		GitCredentialID: v.GitCredentialID, UID: v.UID, GID: v.GID,
		StackID: v.StackID, RestartTargets: v.RestartTargets,
		AutoSync: v.AutoSync, HasWebhook: v.WebhookSecretEnc != "",
		SyncedCommit: v.SyncedCommit, Status: v.Status, LastError: v.LastError,
	}
	if v.StackID != "" {
		if st, err := s.store.StackByID(ctx, v.StackID); err == nil {
			out.StackName = st.Name
		}
	}
	if !v.SyncedAt.IsZero() {
		t := v.SyncedAt
		out.SyncedAt = &t
	}
	if v.Warnings != "" {
		out.Warnings = strings.Split(v.Warnings, "\n")
	}
	return out
}

func (s *Server) handleListVolumeSources(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.VolSourcesView)
	list, err := s.store.ListVolumeSources(r.Context(), global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]volumeSourceView, 0, len(list))
	for _, v := range list {
		out = append(out, s.viewVolumeSource(r.Context(), v))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// volumeSourceSavedResponse is the create/update answer. The webhook secret appears
// exactly once, when it is minted — it is sealed in the database afterwards and never
// handed back.
type volumeSourceSavedResponse struct {
	Source        volumeSourceView `json:"source"`
	WebhookSecret string           `json:"webhook_secret,omitempty"`
}

type volSourceFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    int64  `json:"mode"`
}

type volumeSourceRequest struct {
	EnvID           string `json:"env_id"`
	Volume          string `json:"volume"`
	SourceKind      string `json:"source_kind"` // git | inline; empty = git
	GitURL          string `json:"git_url"`
	GitRef          string `json:"git_ref"`
	GitPath         string `json:"git_path"`
	GitCredentialID string `json:"git_credential_id"`
	// Files is the inline source's content — the set of files delivered into the volume.
	Files          []volSourceFileInput `json:"files"`
	UID            int                  `json:"uid"`
	GID            int                  `json:"gid"`
	StackID        string               `json:"stack_id"`
	RestartTargets string               `json:"restart_targets"`
	AutoSync       bool                 `json:"auto_sync"`
	// Rotate mints a fresh webhook secret. The old one stops working immediately.
	Rotate bool `json:"rotate"`
}

// applyVolumeSourceRequest validates the request and copies it onto the source. Shared by
// create and update, because a validation that exists on one and not the other is a bug
// waiting for whichever path the attacker reads second. It does NOT persist the inline
// files — that needs the source id, so the caller does it after the row write.
func (s *Server) applyVolumeSourceRequest(w http.ResponseWriter, r *http.Request, req *volumeSourceRequest, v *store.VolumeSource) bool {
	if req.UID < 0 || req.GID < 0 {
		httpx.BadRequest(w, r, "uid and gid cannot be negative.")
		return false
	}
	if req.StackID != "" {
		st, err := s.store.StackByID(r.Context(), req.StackID)
		if err != nil {
			httpx.BadRequest(w, r, "No such stack to link.")
			return false
		}
		if st.EnvID != v.EnvID {
			httpx.BadRequest(w, r, "The linked stack runs on a different host than the volume — a deploy there could never mount it.")
			return false
		}
	}

	kind := req.SourceKind
	if kind == "" {
		kind = v.SourceKind // update keeps the existing kind; create falls through to git
	}
	if kind == "" {
		kind = "git"
	}
	switch kind {
	case "git":
		if strings.TrimSpace(req.GitURL) == "" {
			httpx.BadRequest(w, r, "A git volume source needs a repository URL — the repo is the source of truth.")
			return false
		}
		if req.GitCredentialID != "" {
			if _, err := s.store.GitCredentialByID(r.Context(), req.GitCredentialID); err != nil {
				httpx.BadRequest(w, r, "No such git credential.")
				return false
			}
		}
		v.GitURL = strings.TrimSpace(req.GitURL)
		v.GitRef = strings.TrimSpace(req.GitRef)
		v.GitPath = strings.TrimSpace(req.GitPath)
		v.GitCredentialID = req.GitCredentialID
		v.AutoSync = req.AutoSync
	case "inline":
		if len(req.Files) == 0 {
			httpx.BadRequest(w, r, "An inline volume source needs at least one file — that is its content.")
			return false
		}
		files, err := volSourceFilesFromRequest(req.Files)
		if err != nil {
			httpx.BadRequest(w, r, err.Error())
			return false
		}
		// Pre-flight the shared-directory rule while the file set is still in the request:
		// a volume can also carry a certificate delivery (the mixed Traefik dynamic
		// directory), and the two coexist only while their filenames stay disjoint. The
		// sync refuses this too, but refusing at save is the difference between "your edit
		// was rejected, here is why" and storing a file set that can never be delivered and
		// only turning the source red later.
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, f.Path)
		}
		if err := s.refuseDeliveryOwnedNames(r.Context(), v.EnvID, v.Volume, names); err != nil {
			httpx.Fail(w, r, http.StatusConflict, "delivery_owns_file", err.Error())
			return false
		}
		// An inline source has no repository to push to, so git fields and the push-driven
		// webhook are meaningless — clear them rather than store a lie.
		v.GitURL, v.GitRef, v.GitPath, v.GitCredentialID = "", "", "", ""
		v.AutoSync = false
	default:
		httpx.BadRequest(w, r, "The source kind must be git or inline.")
		return false
	}

	v.SourceKind = kind
	v.UID, v.GID = req.UID, req.GID
	v.StackID = req.StackID
	v.RestartTargets = strings.TrimSpace(req.RestartTargets)
	return true
}

// volSourceFilesFromRequest validates the inline files and converts them for storage. Paths
// are the file names in the volume: relative, cleaned, no traversal — the same discipline
// volumes.Write assumes, checked here where the message can name the fix.
func volSourceFilesFromRequest(in []volSourceFileInput) ([]store.VolSourceFile, error) {
	out := make([]store.VolSourceFile, 0, len(in))
	seen := map[string]bool{}
	for _, f := range in {
		p := strings.TrimSpace(f.Path)
		if p == "" {
			return nil, errors.New("every inline file needs a path.")
		}
		if strings.HasPrefix(p, "/") || p != path.Clean(p) || strings.HasPrefix(p, "../") || p == ".." {
			return nil, errors.New("file path " + f.Path + " must be a clean relative path (no leading / and no ..).")
		}
		if seen[p] {
			return nil, errors.New("duplicate file path " + p + ".")
		}
		seen[p] = true
		out = append(out, store.VolSourceFile{Path: p, Content: f.Content, Mode: f.Mode})
	}
	return out, nil
}

// mintWebhookSecret creates the source's webhook secret when auto-sync needs one (or a
// rotation asks for one), returning the plaintext to show exactly once.
func (s *Server) mintWebhookSecret(v *store.VolumeSource, req *volumeSourceRequest) (string, error) {
	// v.AutoSync, not req.AutoSync: apply already resolved it (an inline source is forced
	// off, since there is no repository to push).
	if !v.AutoSync || (v.WebhookSecretEnc != "" && !req.Rotate) {
		return "", nil
	}
	secret, err := randomToken()
	if err != nil {
		return "", err
	}
	sealed, err := s.sealer.Seal(secret)
	if err != nil {
		return "", err
	}
	v.WebhookSecretEnc = sealed
	return secret, nil
}

func (s *Server) handleCreateVolumeSource(w http.ResponseWriter, r *http.Request) {
	var req volumeSourceRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	// The route is body-scoped: the environment arrives in the payload, so the capability
	// check is ours to make, here, before anything else is decided.
	if !s.mayUseEnv(w, r, caps.VolSourcesEdit, req.EnvID) {
		return
	}
	if _, err := s.pool.Get(req.EnvID); err != nil {
		httpx.BadRequest(w, r, "That environment is not connected.")
		return
	}
	if !dockerVolumeName.MatchString(req.Volume) {
		badName(w, r)
		return
	}

	v := &store.VolumeSource{EnvID: req.EnvID, Volume: req.Volume}
	if !s.applyVolumeSourceRequest(w, r, &req, v) {
		return
	}
	secret, err := s.mintWebhookSecret(v, &req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		v.CreatedBy = u.ID
	}
	if err := s.store.CreateVolumeSource(r.Context(), v); err != nil {
		// The unique index speaks here: one source per volume, or two sources would take
		// turns mirror-deleting each other's files.
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			httpx.Fail(w, r, http.StatusBadRequest, "volume_has_source",
				"That volume already has a source. Edit it, or delete it first — two sources on one volume would fight.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	// Inline files need the source id, so they are stored after the row. Validation already
	// ran in applyVolumeSourceRequest, so this only fails on a database error.
	if v.SourceKind == "inline" {
		files, _ := volSourceFilesFromRequest(req.Files)
		if err := s.store.SetVolSourceFiles(r.Context(), v.ID, files); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: v.EnvID, Action: "volsource.create", Target: v.Volume, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"kind": v.SourceKind, "repo": v.GitURL, "path": v.GitPath, "stack_id": v.StackID}),
	})

	// First sync now, in the background — creating a source should not hang the request
	// on a clone and a volume write, but the operator should see it go green in seconds.
	go func(v store.VolumeSource) {
		_ = s.reportVolumeSourceSync(context.WithoutCancel(r.Context()), &v)
	}(*v)

	resp := volumeSourceSavedResponse{Source: s.viewVolumeSource(r.Context(), v)}
	if secret != "" {
		resp.WebhookSecret = secret // shown once
	}
	httpx.JSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetVolumeSource(w http.ResponseWriter, r *http.Request) {
	v, ok := volSourceFrom(r.Context())
	if !ok {
		httpx.Error(w, r, errors.New("api: volume source missing from context"))
		return
	}
	view := s.viewVolumeSource(r.Context(), v)
	// The single-source GET carries the inline files so the editor can load them; the list
	// deliberately does not (contents can be large).
	if v.SourceKind == "inline" {
		files, err := s.store.VolSourceFiles(r.Context(), v.ID)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		view.Files = make([]volSourceFileView, 0, len(files))
		for _, f := range files {
			view.Files = append(view.Files, volSourceFileView{Path: f.Path, Content: f.Content, Mode: f.Mode})
		}
	}
	httpx.JSON(w, http.StatusOK, view)
}

func (s *Server) handleUpdateVolumeSource(w http.ResponseWriter, r *http.Request) {
	v, ok := volSourceFrom(r.Context())
	if !ok {
		httpx.Error(w, r, errors.New("api: volume source missing from context"))
		return
	}
	var req volumeSourceRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	// Switching the source kind is a deliberate, validated transition — only inline → git, the
	// mirror of the stack switch. A git source's files ARE the repository, so there is nothing to
	// import back into an inline set; snapshotting the last synced tree would silently fork the
	// volume from the repo it claims to track. Refuse the reverse rather than half-do it. Captured
	// before applyVolumeSourceRequest, which mutates v.SourceKind in place.
	switching := req.SourceKind != "" && req.SourceKind != v.SourceKind
	if switching && (v.SourceKind != "inline" || req.SourceKind != "git") {
		httpx.BadRequest(w, r,
			"Only an inline volume source can be switched to git. A git-backed source keeps its "+
				"files in the repository, so there is nothing to convert back to inline.")
		return
	}

	// EnvID and Volume are not updatable: retargeting a source would strand the old
	// volume with a manifest nothing owns. Delete and recreate, so both halves are
	// explicit. The request's env/volume fields are simply ignored here.
	if !s.applyVolumeSourceRequest(w, r, &req, v) {
		return
	}

	// Prove the new git source is reachable and its subtree resolves BEFORE committing the switch —
	// a typo in URL/ref/path is the operator's mistake, so it is a 400 now, not a red status after
	// the inline files are already gone.
	if switching {
		if err := s.probeVolumeSourceGit(r.Context(), v); err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "git_unreachable", err.Error())
			return
		}
	}

	secret, err := s.mintWebhookSecret(v, &req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.store.UpdateVolumeSource(r.Context(), v); err != nil {
		httpx.Error(w, r, err)
		return
	}
	switch {
	case switching:
		// The inline files are dead data on a git source now — the repo is the source of truth, and
		// the volume's contents are replaced by the sync below. Clear them so nothing stale lingers.
		if err := s.store.SetVolSourceFiles(r.Context(), v.ID, nil); err != nil {
			httpx.Error(w, r, err)
			return
		}
	case v.SourceKind == "inline":
		files, _ := volSourceFilesFromRequest(req.Files)
		if err := s.store.SetVolSourceFiles(r.Context(), v.ID, files); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: v.EnvID, Action: "volsource.update", Target: v.Volume, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"kind": v.SourceKind, "repo": v.GitURL, "path": v.GitPath, "stack_id": v.StackID}),
	})

	// The edit may have retargeted ref or subtree; reconcile in the background. The hash
	// makes an unchanged edit cost one clone and no Docker calls.
	go func(v store.VolumeSource) {
		_ = s.reportVolumeSourceSync(context.WithoutCancel(r.Context()), &v)
	}(*v)

	resp := volumeSourceSavedResponse{Source: s.viewVolumeSource(r.Context(), v)}
	if secret != "" {
		resp.WebhookSecret = secret // shown once
	}
	httpx.JSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteVolumeSource(w http.ResponseWriter, r *http.Request) {
	v, ok := volSourceFrom(r.Context())
	if !ok {
		httpx.Error(w, r, errors.New("api: volume source missing from context"))
		return
	}
	// The volume and its contents are left in place: the consumer may still be running,
	// and Daffa yanking config out from under a live proxy is a worse surprise than a
	// stale file. The volume becomes an ordinary volume, removable through volumes.edit
	// when the operator decides.
	if err := s.store.DeleteVolumeSource(r.Context(), v.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: v.EnvID, Action: "volsource.delete", Target: v.Volume, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

func (s *Server) handleSyncVolumeSource(w http.ResponseWriter, r *http.Request) {
	v, ok := volSourceFrom(r.Context())
	if !ok {
		httpx.Error(w, r, errors.New("api: volume source missing from context"))
		return
	}
	// Synchronous, and forced: "sync now" is the button an operator presses while looking
	// at a red status, and it should answer with the outcome, not with "started".
	v.SyncedHash = ""
	if err := s.reportVolumeSourceSync(r.Context(), v); err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "sync_failed", err.Error())
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: v.EnvID, Action: "volsource.sync", Target: v.Volume, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, s.viewVolumeSource(r.Context(), v))
}

// handleVolumeSourceWebhook is the push-triggered sync: the stack webhook's twin, with
// the same posture — outside /api/, authenticated by the HMAC alone, and it confirms
// nothing to an unsigned caller.
func (s *Server) handleVolumeSourceWebhook(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.VolumeSourceByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
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

	secret, err := s.sealer.Open(v.WebhookSecretEnc)
	if err != nil || secret == "" {
		s.auditVolSourceWebhook(r, v, "denied", "auto-sync is not enabled for this source")
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "No such webhook.")
		return
	}
	if err := verifySignature(r, body, secret); err != nil {
		s.auditVolSourceWebhook(r, v, "denied", err.Error())
		httpx.Fail(w, r, http.StatusUnauthorized, "bad_signature",
			"The signature does not match. Check the secret configured on the git server.")
		return
	}

	if isPing(r) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	if !v.AutoSync {
		httpx.JSON(w, http.StatusOK, map[string]string{
			"status": "ignored", "reason": "auto-sync is turned off for this source",
		})
		return
	}

	push, err := parsePush(body)
	if err != nil {
		httpx.BadRequest(w, r, "The payload is not a push event this understands.")
		return
	}
	if push.Ref != "" && v.GitRef != "" && !refMatches(push.Ref, v.GitRef) {
		httpx.JSON(w, http.StatusOK, map[string]string{
			"status": "ignored", "reason": "push was to " + push.Ref + ", this source tracks " + v.GitRef,
		})
		return
	}
	// Nothing under the subtree was touched. (An empty file list — some forges omit it on
	// large pushes — syncs rather than silently doing nothing; the hash makes a redundant
	// sync cost one clone.)
	if len(push.Files) > 0 && !touchesSubtree(push.Files, v.GitPath) {
		httpx.JSON(w, http.StatusOK, map[string]string{
			"status": "ignored", "reason": "no file under " + path.Clean(v.GitPath) + " was changed",
		})
		return
	}

	// Background: a clone plus a fan-out of volume writes is longer than a forge's
	// webhook timeout wants to wait, and the source row records the outcome either way.
	go func(v store.VolumeSource) {
		_ = s.reportVolumeSourceSync(context.WithoutCancel(r.Context()), &v)
	}(*v)

	s.auditVolSourceWebhook(r, v, "ok", "")
	slog.Info("webhook volume sync started", "volume", v.Volume, "ref", push.Ref)
	httpx.JSON(w, http.StatusAccepted, map[string]string{"status": "syncing"})
}

func (s *Server) auditVolSourceWebhook(r *http.Request, v *store.VolumeSource, outcome, reason string) {
	detail := map[string]string{"ip": s.clientIP(r)}
	if reason != "" {
		detail["reason"] = reason
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: v.EnvID, Action: "volsource.webhook", Target: v.Volume,
		Outcome: outcome, Detail: store.AuditDetail(detail),
	})
}

// touchesSubtree reports whether any pushed file lives under the source's subtree.
func touchesSubtree(files []string, subtree string) bool {
	dir := path.Clean(strings.TrimPrefix(subtree, "/"))
	if dir == "" || dir == "." {
		return true // the whole repository is the subtree
	}
	for _, f := range files {
		f = path.Clean(strings.TrimPrefix(f, "/"))
		if f == dir || strings.HasPrefix(f, dir+"/") {
			return true
		}
	}
	return false
}
