package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// Keyrings: rotatable application encryption keys. See docs/keyrings.md.
//
// Every view here is secret-free by construction — a version is its id (the kid), its
// state and its age. The material is sealed at generation and thereafter exists in
// plaintext only inside delivery volumes; no API response ever carries it, so there is
// no has_material boolean either: a version that exists has material, always.

type keyringVersionView struct {
	ID        string    `json:"id"` // the kid applications store beside their ciphertext
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

type keyringView struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	RotateDays int                  `json:"rotate_days"`
	CreatedAt  time.Time            `json:"created_at"`
	Versions   []keyringVersionView `json:"versions"` // newest first, retired included
}

func viewKeyring(k *store.Keyring, versions []*store.KeyringVersion) keyringView {
	v := keyringView{
		ID: k.ID, Name: k.Name, RotateDays: k.RotateDays, CreatedAt: k.CreatedAt,
		Versions: make([]keyringVersionView, 0, len(versions)),
	}
	for _, kv := range versions {
		v.Versions = append(v.Versions, keyringVersionView{
			ID: kv.ID, State: kv.State, CreatedAt: kv.CreatedAt,
		})
	}
	return v
}

func (s *Server) keyringView(r *http.Request, k *store.Keyring) (keyringView, error) {
	versions, err := s.store.KeyringVersions(r.Context(), k.ID)
	if err != nil {
		return keyringView{}, err
	}
	return viewKeyring(k, versions), nil
}

func (s *Server) handleListKeyrings(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListKeyrings(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]keyringView, 0, len(list))
	for _, k := range list {
		v, err := s.keyringView(r, k)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type keyringRequest struct {
	Name       string `json:"name"`
	RotateDays int    `json:"rotate_days"` // 0 = manual rotation only
}

func (s *Server) handleCreateKeyring(w http.ResponseWriter, r *http.Request) {
	var req keyringRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !certName.MatchString(req.Name) {
		badName(w, r)
		return
	}
	if req.RotateDays < 0 {
		httpx.BadRequest(w, r, "The rotation schedule cannot be negative. Use 0 for manual rotation.")
		return
	}

	k := &store.Keyring{Name: req.Name, RotateDays: req.RotateDays}
	if u, ok := auth.UserFrom(r.Context()); ok {
		k.CreatedBy = u.ID
	}
	if err := s.store.CreateKeyring(r.Context(), k); err != nil {
		if store.IsDuplicate(err) {
			httpx.Fail(w, r, http.StatusBadRequest, "name_taken", "A keyring with that name already exists.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	// Seed the first version immediately: a keyring with nothing to encrypt with is an
	// invalid state, and every delivery of it would fail with "rotate it first".
	if _, err := s.rotateKeyring(r.Context(), k); err != nil {
		// Leave no half-made keyring behind — the operator will retry the create.
		_ = s.store.DeleteKeyring(r.Context(), k.ID)
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "keyring.create", Target: k.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"rotate_days": k.RotateDays}),
	})

	v, err := s.keyringView(r, k)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// rotateKeyring mints, seals and appends a new active version. Shared by create, the
// rotate endpoint and the worker's scheduled rotation.
func (s *Server) rotateKeyring(ctx context.Context, k *store.Keyring) (*store.KeyringVersion, error) {
	material, err := newKeyringMaterial()
	if err != nil {
		return nil, err
	}
	sealed, err := s.sealer.Seal(material)
	if err != nil {
		return nil, fmt.Errorf("keyrings: sealing the new version: %w", err)
	}
	return s.store.RotateKeyring(ctx, k.ID, sealed)
}

func (s *Server) handleRotateKeyring(w http.ResponseWriter, r *http.Request) {
	k, err := s.store.KeyringByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_keyring", "No such keyring.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	nv, err := s.rotateKeyring(r.Context(), k)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "keyring.rotate", Target: k.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"version": nv.ID}),
	})
	s.resyncKeyringDeliveries(r.Context())

	v, err := s.keyringView(r, k)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// keyringUpdateRequest carries the one editable field. The name is deliberately absent —
// it is the filename deliveries have already written into volumes.
type keyringUpdateRequest struct {
	RotateDays int `json:"rotate_days"` // 0 = manual rotation only
}

func (s *Server) handleUpdateKeyring(w http.ResponseWriter, r *http.Request) {
	k, err := s.store.KeyringByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_keyring", "No such keyring.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Only the schedule is editable. The name is the filename deliveries write, and a
	// rename would leave every volume holding a stale <old-name>.json that looks current.
	var req keyringUpdateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if req.RotateDays < 0 {
		httpx.BadRequest(w, r, "The rotation schedule cannot be negative. Use 0 for manual rotation.")
		return
	}
	k.RotateDays = req.RotateDays
	if err := s.store.UpdateKeyring(r.Context(), k); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "keyring.update", Target: k.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"rotate_days": k.RotateDays}),
	})

	v, err := s.keyringView(r, k)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

func (s *Server) handleRetireKeyringVersion(w http.ResponseWriter, r *http.Request) {
	k, err := s.store.KeyringByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_keyring", "No such keyring.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	kv, err := s.store.KeyringVersionByID(r.Context(), r.PathValue("vid"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && kv.KeyringID != k.ID) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_version", "No such version on that keyring.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The store's WHERE clause is the real guard (a race with rotate must not slip
	// through); these checks exist to give the refusal a reason instead of a shrug.
	ok, err := s.store.RetireKeyringVersion(r.Context(), kv.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !ok {
		switch kv.State {
		case store.KeyringVersionActive:
			httpx.Fail(w, r, http.StatusBadRequest, "version_active",
				"That is the active version — rotate the keyring first, then retire it.")
		case store.KeyringVersionRetired:
			httpx.Fail(w, r, http.StatusBadRequest, "version_retired", "That version is already retired.")
		default:
			httpx.Fail(w, r, http.StatusConflict, "retire_raced", "The version changed state underneath this request. Reload and retry.")
		}
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "keyring.retire", Target: k.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"version": kv.ID}),
	})
	// The next sync drops the version from every volume — that is what retirement IS.
	s.resyncKeyringDeliveries(r.Context())

	v, err := s.keyringView(r, k)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteKeyring(w http.ResponseWriter, r *http.Request) {
	k, err := s.store.KeyringByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_keyring", "No such keyring.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Refuse, don't orphan: a delivery still carrying this keyring means some volume —
	// and probably some application — still depends on it.
	n, err := s.store.KeyringInUse(r.Context(), k.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n > 0 {
		phrase := fmt.Sprintf("%d deliveries still carry", n)
		if n == 1 {
			phrase = "A delivery still carries"
		}
		httpx.Fail(w, r, http.StatusBadRequest, "keyring_in_use",
			phrase+" this keyring. Delete them first — the volumes keep their last-written files.")
		return
	}

	if err := s.store.DeleteKeyring(r.Context(), k.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "keyring.delete", Target: k.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
