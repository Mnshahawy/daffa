package api

import (
	"context"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/backups"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

type jobView struct {
	ID        string `json:"id"`
	EnvID     string `json:"env_id"`
	Name      string `json:"name"`
	Container string `json:"container,omitempty"`
	Engine    string `json:"engine"`
	Databases string `json:"databases,omitempty"`
	DBUser    string `json:"db_user,omitempty"`
	Schedule  string `json:"schedule,omitempty"`

	Volume         string `json:"volume,omitempty"`
	StopContainers string `json:"stop_containers,omitempty"`
	ExcludePaths   string `json:"exclude_paths,omitempty"` // newline-separated; volume engine only

	StorageID   string `json:"storage_id"`
	StorageName string `json:"storage_name"` // resolved, so the list needs no second call
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix,omitempty"`

	Encryption string   `json:"encryption"` // age | none
	KeyIDs     []string `json:"key_ids,omitempty"`
	KeyNames   []string `json:"key_names,omitempty"` // resolved, so the list needs no second call

	Enabled bool      `json:"enabled"`
	Last    *runView2 `json:"last_run,omitempty"`
}

type runView2 struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Trigger   string    `json:"trigger"`
	Bytes     int64     `json:"bytes"`
	ObjectKey string    `json:"object_key,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

func viewBackupRun(r *store.BackupRun) *runView2 {
	return &runView2{
		ID: r.ID, Status: r.Status, Trigger: r.Trigger, Bytes: r.Bytes,
		ObjectKey: r.ObjectKey, Error: r.Error, StartedAt: r.StartedAt,
	}
}

func (s *Server) handleListBackupJobs(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.BackupsView)
	jobs, err := s.store.ListBackupJobs(r.Context(), global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// One lookup for every job's key names — the keys an operator must hold to restore.
	keyName := map[string]string{}
	if keys, err := s.store.ListEncryptionKeys(r.Context()); err == nil {
		for _, k := range keys {
			keyName[k.ID] = k.Name
		}
	}

	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		// Nothing sealed is ever rendered: not the S3 secret, not the DB password. The
		// encryption KEYS are public recipients and are named deliberately — an operator
		// needs to know which key will be required to restore.
		v := jobView{
			ID: j.ID, EnvID: j.EnvID, Name: j.Name, Container: j.Container, Engine: j.Engine,
			Databases: j.Databases, DBUser: j.DBUser, Schedule: j.Schedule,
			Volume: j.Volume, StopContainers: j.StopContainers, ExcludePaths: j.ExcludePaths,
			StorageID: j.StorageID, Prefix: j.Prefix,
			Encryption: j.Encryption, KeyIDs: j.KeyIDs, Enabled: j.Enabled,
		}
		for _, id := range j.KeyIDs {
			if n, ok := keyName[id]; ok {
				v.KeyNames = append(v.KeyNames, n)
			}
		}
		if t, err := s.store.StorageTargetByID(r.Context(), j.StorageID); err == nil {
			v.StorageName, v.Bucket = t.Name, t.Bucket
		}
		if last, err := s.store.LastBackupRun(r.Context(), j.ID); err == nil {
			v.Last = viewBackupRun(last)
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type jobRequest struct {
	EnvID      string `json:"env_id"`
	Name       string `json:"name"`
	Container  string `json:"container"`
	Engine     string `json:"engine"`
	Databases  string `json:"databases"`
	DBUser     string `json:"db_user"`
	DBPassword string `json:"db_password"`
	Schedule   string `json:"schedule"`

	// The volume engine's fields; unused (and cleared) for a database engine.
	Volume         string `json:"volume"`
	StopContainers string `json:"stop_containers"`
	ExcludePaths   string `json:"exclude_paths"` // newline-separated paths dropped from the snapshot

	StorageID string `json:"storage_id"`
	Prefix    string `json:"prefix"`

	Encryption string   `json:"encryption"`
	KeyIDs     []string `json:"key_ids"`
}

// sanitizeExcludePaths normalizes the newline-separated exclude list into its stored
// form and refuses any pattern that escapes the volume. An escaping pattern (absolute,
// "..", or a "../" prefix after cleaning) can only match nothing — but a "backup" that
// silently ignored what an operator typed is worse than one that refuses and says why,
// so it is a 400 naming the offender rather than a quiet drop. Returns the cleaned list
// (blanks removed, one path per line) and, if a pattern was illegal, the raw pattern.
func sanitizeExcludePaths(list string) (cleaned, bad string) {
	var out []string
	for _, raw := range strings.Split(list, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		p := path.Clean(strings.TrimPrefix(raw, "./"))
		if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
			return "", raw
		}
		out = append(out, p)
	}
	return strings.Join(out, "\n"), ""
}

func (s *Server) handleCreateBackupJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	// The host arrives in the BODY, so no middleware could have checked it. Without this,
	// backups.edit on ANY host would mean backups.edit on EVERY host — and a backup job can
	// exec into a container and read a whole database out of it.
	if !s.mayUseEnv(w, r, caps.BackupsEdit, req.EnvID) {
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpx.BadRequest(w, r, "A name is required.")
		return
	}
	switch {
	case req.Engine == string(backups.Volume):
		// The volume engine's subject is a volume, not a container; the database fields
		// mean nothing to it and are dropped rather than stored as dead weight.
		if !dockerVolumeName.MatchString(req.Volume) {
			badName(w, r)
			return
		}
		req.Container, req.Databases, req.DBUser, req.DBPassword = "", "", "", ""
		req.StopContainers = strings.TrimSpace(req.StopContainers)
		cleaned, badPath := sanitizeExcludePaths(req.ExcludePaths)
		if badPath != "" {
			httpx.BadRequest(w, r, "Exclude path "+badPath+" must stay inside the volume — use a path relative to the volume root, like \"cache\" or \"tmp/sessions\", not an absolute path or \"..\".")
			return
		}
		req.ExcludePaths = cleaned
	case backups.ValidEngine(backups.Engine(req.Engine)):
		if req.Container == "" {
			httpx.BadRequest(w, r, "A container is required.")
			return
		}
		req.Volume, req.StopContainers, req.ExcludePaths = "", "", ""
	default:
		httpx.BadRequest(w, r, "The engine must be postgres, mysql, mongodb, or volume.")
		return
	}
	if _, err := s.pool.Get(req.EnvID); err != nil {
		httpx.BadRequest(w, r, "That environment is not connected.")
		return
	}

	// Validate the schedule here rather than discovering it is nonsense at midnight.
	if req.Schedule != "" {
		if _, err := cron.ParseStandard(req.Schedule); err != nil {
			httpx.BadRequest(w, r, "That is not a valid cron expression (e.g. \"0 3 * * *\" for 03:00 UTC daily).")
			return
		}
	}

	if req.Encryption != "none" {
		req.Encryption = "age"
		// Jobs reference named encryption keys now; raw recipient strings (and the guard
		// against pasting a private key) live with the keys feature, where they belong.
		if len(req.KeyIDs) == 0 {
			httpx.BadRequest(w, r,
				"Encryption is on, so at least one encryption key is required. "+
					"Generate or import one under Certificates → Encryption keys — and keep the private half somewhere safe; Daffa cannot read these backups without it.")
			return
		}
		for _, id := range req.KeyIDs {
			if _, err := s.store.EncryptionKeyByID(r.Context(), id); err != nil {
				httpx.BadRequest(w, r, "One of the selected encryption keys no longer exists.")
				return
			}
		}
	} else {
		req.KeyIDs = nil
	}

	// The bucket is a storage target now: chosen from a list that was already tested when
	// it was created, rather than five fields retyped per job.
	if _, err := s.store.StorageTargetByID(r.Context(), req.StorageID); err != nil {
		httpx.BadRequest(w, r, "Choose a storage target. Add one under Settings → Storage.")
		return
	}

	dbPass, err := s.sealer.Seal(req.DBPassword)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	job := &store.BackupJob{
		EnvID: req.EnvID, Name: req.Name, Container: req.Container, Engine: req.Engine,
		Databases: req.Databases, DBUser: req.DBUser, DBPasswordEnc: dbPass,
		Schedule: req.Schedule,
		Volume:   req.Volume, StopContainers: req.StopContainers, ExcludePaths: req.ExcludePaths,
		StorageID: req.StorageID, Prefix: strings.Trim(strings.TrimSpace(req.Prefix), "/"),
		Encryption: req.Encryption, KeyIDs: req.KeyIDs,
		Enabled: true,
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		job.CreatedBy = u.ID
	}

	if err := s.store.CreateBackupJob(r.Context(), job); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "A backup job with that name already exists.")
		return
	}
	s.rebuildSchedule(r.Context())

	s.audit(r.Context(), store.AuditEntry{
		EnvID: job.EnvID, Action: "backup.create", Target: job.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"engine": job.Engine, "encrypted": job.Encrypted()}),
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"id": job.ID})
}

// backupJob returns the job this request is about.
//
// The scopeJob middleware already resolved it AND checked that the caller holds the route's
// capability on the job's host, so this is a context read, not a query. The fallback exists
// only so a handler mounted without that middleware fails closed rather than silently
// skipping the check.
func (s *Server) backupJob(w http.ResponseWriter, r *http.Request) (*store.BackupJob, bool) {
	if j, ok := jobFrom(r.Context()); ok {
		return j, true
	}
	httpx.Fail(w, r, http.StatusNotFound, "no_such_job", "No such backup job.")
	return nil, false
}

func (s *Server) handleDeleteBackupJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}

	// Deleting a job stops future backups. It does NOT delete the snapshots already in
	// the bucket — those are the whole point, and Daffa will not touch them.
	if err := s.store.DeleteBackupJob(r.Context(), job.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.rebuildSchedule(r.Context())

	s.audit(r.Context(), store.AuditEntry{
		EnvID: job.EnvID, Action: "backup.delete", Target: job.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleToggleBackupJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}

	enabled := !job.Enabled
	if err := s.store.SetBackupJobEnabled(r.Context(), job.ID, enabled); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.rebuildSchedule(r.Context())

	s.audit(r.Context(), store.AuditEntry{
		EnvID: job.EnvID, Action: "backup.toggle", Target: job.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"enabled": enabled}),
	})
	httpx.JSON(w, http.StatusOK, map[string]bool{"enabled": enabled})
}

// handleRunBackup kicks a job off now. It returns immediately: a database dump can take
// hours, and an HTTP request is not a place to wait for one.
func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}

	var userID string
	if u, ok := auth.UserFrom(r.Context()); ok {
		userID = u.ID
	}

	// The backup MUST NOT inherit the request's context. This handler returns
	// immediately (a dump can take hours), which cancels r.Context() — and a backup
	// wired to it would be killed a microsecond after it was asked for, having recorded
	// nothing. Detach before handing it to the goroutine.
	go s.runBackup(context.WithoutCancel(r.Context()), job.ID, "manual", userID)

	httpx.JSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) handleBackupRuns(w http.ResponseWriter, r *http.Request) {
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}

	runs, err := s.store.ListBackupRuns(r.Context(), job.ID, 20)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]*runView2, 0, len(runs))
	for _, run := range runs {
		out = append(out, viewBackupRun(run))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// handleSnapshots lists what is actually in the bucket — the only honest answer to "do I
// have a backup?", since a run record only says what Daffa believes it did.
func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}

	_, dst, err := s.jobConfig(r.Context(), job)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	list, err := backups.List(r.Context(), dst, 50)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "storage_unreachable", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

// keyUnderPrefix reports whether an object key belongs to a job whose snapshots live under
// prefix. It mirrors the normalisation Backups.List and objectKey use (trim slashes, then a
// trailing separator), and rejects any ".." segment so a crafted key cannot climb out of the
// prefix it claims to be under. An empty prefix confines to nothing: that job's scope IS the
// whole bucket.
func keyUnderPrefix(key, prefix string) bool {
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return false
		}
	}
	p := strings.Trim(prefix, "/")
	if p == "" {
		return true
	}
	return strings.HasPrefix(key, p+"/")
}

// handleSnapshotDownload streams a snapshot back to the CLI, still encrypted.
//
// The server has no key and does not decrypt: it is a conduit. This is what lets the
// operator's machine be the only place the private key ever exists.
func (s *Server) handleSnapshotDownload(w http.ResponseWriter, r *http.Request) {
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		httpx.BadRequest(w, r, "A snapshot key is required.")
		return
	}

	_, dst, err := s.jobConfig(r.Context(), job)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Confine the download to THIS job's prefix. Fetch hands the key straight to GetObject with
	// only the bucket, so without this a caller who holds backups.download on one job could pull
	// another job's snapshots out of a shared bucket by naming their (enumerable) key. The list
	// path is already prefix-scoped; the download must match it. An empty prefix means the job
	// legitimately owns the whole bucket, exactly as its listing does.
	if !keyUnderPrefix(key, dst.Prefix) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_snapshot",
			"That snapshot is not part of this backup job.")
		return
	}

	obj, err := backups.Fetch(r.Context(), dst, key)
	if err != nil {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_snapshot", err.Error())
		return
	}
	defer obj.Close()

	s.audit(r.Context(), store.AuditEntry{
		EnvID: job.EnvID, Action: "backup.download", Target: key, Outcome: "ok",
	})

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, obj)
}

// handleRestore takes an ALREADY-DECRYPTED dump on the request body and pipes it into
// the database.
//
// The CLI decrypts on the operator's machine and streams the plaintext here, because the
// server is the only thing that can reach the container. The key stays with the person.
//
// This overwrites a live database. It is admin-only, requires the job name echoed back,
// and is audited before and after.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}

	if r.URL.Query().Get("confirm") != job.Name {
		httpx.Fail(w, r, http.StatusBadRequest, "confirm_required",
			"Restoring overwrites the live database. Pass ?confirm=<job name> to proceed.")
		return
	}

	if job.Engine == string(backups.Volume) {
		s.restoreVolume(w, r, job)
		return
	}

	node, containerName, err := s.backupNode(r.Context(), job.EnvID, job.Container)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "env_unreachable", err.Error())
		return
	}

	spec, _, err := s.jobConfig(r.Context(), job)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: job.EnvID, Action: "backup.restore", Target: job.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"event": "started", "container": job.Container}),
	})

	// No body size limit here, deliberately: this is a database, and capping it would
	// silently truncate the restore. The route is admin-only.
	output, err := backups.Load(r.Context(), node, containerName, spec, r.Body)

	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: job.EnvID, Action: "backup.restore", Target: job.Name, Outcome: outcome,
		Detail: store.AuditDetail(map[string]string{"event": "finished", "error": errText(err)}),
	})

	if err != nil {
		httpx.JSON(w, http.StatusBadGateway, map[string]string{
			"code": "restore_failed", "message": err.Error(), "output": tail(output, 4000),
		})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok", "output": tail(output, 4000)})
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
