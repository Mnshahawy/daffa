package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/backups"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// A storage target is a bucket, configured once and shared by any number of backup jobs.
// The secret key is sealed and never leaves the server — not even to the admin who typed
// it.

type storageView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	KeyID    string `json:"key_id"`
	InUse    int    `json:"in_use"`
}

func (s *Server) handleListStorage(w http.ResponseWriter, r *http.Request) {
	targets, err := s.store.ListStorageTargets(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]storageView, 0, len(targets))
	for _, t := range targets {
		n, _ := s.store.StorageTargetInUse(r.Context(), t.ID)
		out = append(out, storageView{
			ID: t.ID, Name: t.Name, Endpoint: t.Endpoint, Region: t.Region,
			Bucket: t.Bucket, KeyID: t.KeyID, InUse: n,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

type storageRequest struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	KeyID    string `json:"key_id"`
	Secret   string `json:"secret"`
}

func (s *Server) handleCreateStorage(w http.ResponseWriter, r *http.Request) {
	var req storageRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if err := validStorage(&req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	// Prove it works before saving it. A bucket that cannot be reached is not a
	// configuration, it is a future 3am surprise.
	if err := backups.CheckDestination(r.Context(), backups.Destination{
		Endpoint: req.Endpoint, Region: req.Region, Bucket: req.Bucket,
		KeyID: req.KeyID, Secret: req.Secret,
	}); err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "storage_unreachable", err.Error())
		return
	}

	sealed, err := s.sealer.Seal(req.Secret)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	t := &store.StorageTarget{
		Name: req.Name, Endpoint: req.Endpoint, Region: req.Region, Bucket: req.Bucket,
		KeyID: req.KeyID, SecretEnc: sealed,
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		t.CreatedBy = u.ID
	}
	if err := s.store.CreateStorageTarget(r.Context(), t); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "A storage target with that name already exists.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "storage.create", Target: t.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"bucket": t.Bucket, "endpoint": t.Endpoint}),
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"id": t.ID})
}

func (s *Server) handleUpdateStorage(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.StorageTargetByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_target", "No such storage target.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req storageRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if err := validStorage(&req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	// An empty secret means "leave it alone". The UI cannot show it back, so requiring
	// it on every edit would mean re-typing a credential to change a bucket name — and
	// people would paste the wrong one.
	secret := req.Secret
	if secret == "" {
		secret, err = s.sealer.Open(t.SecretEnc)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
	}

	if err := backups.CheckDestination(r.Context(), backups.Destination{
		Endpoint: req.Endpoint, Region: req.Region, Bucket: req.Bucket,
		KeyID: req.KeyID, Secret: secret,
	}); err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "storage_unreachable", err.Error())
		return
	}

	sealed, err := s.sealer.Seal(secret)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	t.Name, t.Endpoint, t.Region = req.Name, req.Endpoint, req.Region
	t.Bucket, t.KeyID, t.SecretEnc = req.Bucket, req.KeyID, sealed

	if err := s.store.UpdateStorageTarget(r.Context(), t); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "storage.update", Target: t.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteStorage(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.StorageTargetByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_target", "No such storage target.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Refuse rather than orphan: a backup job whose bucket vanished would fail at its
	// next run, which is the worst possible moment to learn about it.
	n, err := s.store.StorageTargetInUse(r.Context(), t.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n > 0 {
		httpx.Fail(w, r, http.StatusConflict, "in_use",
			"That storage target is used by backup jobs. Point them elsewhere first.")
		return
	}

	if err := s.store.DeleteStorageTarget(r.Context(), t.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "storage.delete", Target: t.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func validStorage(req *storageRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	req.Bucket = strings.TrimSpace(req.Bucket)
	if req.Region == "" {
		req.Region = "auto" // what R2 wants; harmless elsewhere
	}
	if req.Name == "" || req.Endpoint == "" || req.Bucket == "" || req.KeyID == "" {
		return errors.New("A name, endpoint, bucket and access key are required.")
	}
	return nil
}
