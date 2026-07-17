package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
	"github.com/Mnshahawy/daffa/internal/volumes"
)

// A keyring delivery keeps a keyring's live versions current inside a named volume on a
// host — the cert-delivery machinery with a different payload (docs/keyrings.md §4). Two
// files per keyring:
//
//   <name>.json         every live version, and which one is current
//   <name>.current.key  the active version's raw 32 bytes, for single-key consumers
//
// One JSON document rather than a file per version, on purpose: the consumer does one
// read; the tar copy replaces it atomically from the reader's point of view; and a retired
// version simply stops appearing in the next write, so retirement never needs a file
// deletion and never strands a stale v3.key looking current.
//
// Unlike certs there is no mount-path convention to document: tls.yml had to hardcode
// paths because Traefik resolves them, but the keyring JSON contains no paths at all, so
// the consumer may mount the volume anywhere.

// newKeyringMaterial mints one version's key: 32 random bytes, base64. The encoded string
// is both what the sealer stores and what the JSON's "material" field carries — the raw
// bytes exist only in <name>.current.key.
func newKeyringMaterial() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("keyrings: generating key material: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

type keyringDeliveryView struct {
	ID             string     `json:"id"`
	KeyringID      string     `json:"keyring_id"`
	KeyringName    string     `json:"keyring_name,omitempty"`
	EnvID          string     `json:"env_id"`
	EnvName        string     `json:"env_name,omitempty"`
	Volume         string     `json:"volume"`
	UID            int        `json:"uid"`
	GID            int        `json:"gid"`
	RestartTargets string     `json:"restart_targets,omitempty"`
	Status         string     `json:"status"`
	LastError      string     `json:"last_error,omitempty"`
	SyncedAt       *time.Time `json:"synced_at,omitempty"`
}

func (s *Server) viewKeyringDelivery(ctx context.Context, d *store.KeyringDelivery) keyringDeliveryView {
	v := keyringDeliveryView{
		ID: d.ID, KeyringID: d.KeyringID, EnvID: d.EnvID, EnvName: s.envName(ctx, d.EnvID),
		Volume: d.Volume, UID: d.UID, GID: d.GID, RestartTargets: d.RestartTargets,
		Status: d.Status, LastError: d.LastError,
	}
	if k, err := s.store.KeyringByID(ctx, d.KeyringID); err == nil {
		v.KeyringName = k.Name
	}
	if !d.SyncedAt.IsZero() {
		t := d.SyncedAt
		v.SyncedAt = &t
	}
	return v
}

func (s *Server) handleListKeyringDeliveries(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.KeyringsView)
	list, err := s.store.ListKeyringDeliveries(r.Context(), global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]keyringDeliveryView, 0, len(list))
	for _, d := range list {
		out = append(out, s.viewKeyringDelivery(r.Context(), d))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type keyringDeliveryRequest struct {
	KeyringID string `json:"keyring_id"`
	EnvID     string `json:"env_id"`
	Volume    string `json:"volume"`
	UID       int    `json:"uid"`
	GID       int    `json:"gid"`
	// Space-separated container names bounced after each changed sync, for consumers
	// that cannot hot-reload.
	RestartTargets string `json:"restart_targets"`
}

func (s *Server) handleCreateKeyringDelivery(w http.ResponseWriter, r *http.Request) {
	var req keyringDeliveryRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	// No mayUseEnv here, deliberately: keyrings.edit is global-only, so whoever passed the
	// route guard holds it everywhere — there is no narrower grant to escape.
	if _, err := s.pool.Get(req.EnvID); err != nil {
		httpx.BadRequest(w, r, "That environment is not connected.")
		return
	}
	if _, err := s.store.KeyringByID(r.Context(), req.KeyringID); err != nil {
		httpx.BadRequest(w, r, "No such keyring.")
		return
	}
	if req.Volume != "" && !certName.MatchString(req.Volume) {
		badName(w, r)
		return
	}

	d := &store.KeyringDelivery{
		KeyringID: req.KeyringID, EnvID: req.EnvID, Volume: req.Volume,
		UID: req.UID, GID: req.GID,
		RestartTargets: strings.TrimSpace(req.RestartTargets),
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		d.CreatedBy = u.ID
	}
	if err := s.store.CreateKeyringDelivery(r.Context(), d); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "keyring.deliver", Target: d.Volume, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"keyring_id": d.KeyringID}),
	})

	// First sync now, in the background — creating a delivery should not hang the request
	// on a volume write, but the operator should see it go green within seconds.
	go func(d store.KeyringDelivery) {
		ctx := context.WithoutCancel(r.Context())
		s.reportKeyringDeliverySync(ctx, &d)
	}(*d)

	httpx.JSON(w, http.StatusOK, s.viewKeyringDelivery(r.Context(), d))
}

func (s *Server) handleSyncKeyringDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.KeyringDeliveryByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_delivery", "No such delivery.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Synchronous, and forced: "sync now" is the button an operator presses while looking
	// at a red status, and it should answer with the outcome, not with "started".
	d.SyncedHash = ""
	if err := s.reportKeyringDeliverySync(r.Context(), d); err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "sync_failed", err.Error())
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "keyring.sync", Target: d.Volume, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, s.viewKeyringDelivery(r.Context(), d))
}

func (s *Server) handleDeleteKeyringDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.KeyringDeliveryByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_delivery", "No such delivery.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The volume and its contents are left in place: Daffa deleting key material out from
	// under a running application is a worse surprise than a stale file — worse here than
	// for certs, because there is no re-issuing the data the app encrypted with it. The
	// volume is the operator's to remove.
	if err := s.store.DeleteKeyringDelivery(r.Context(), d.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "keyring.undeliver", Target: d.Volume, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── the reconciler ──────────────────────────────────────────────────────────────

// resyncKeyringDeliveries sweeps every delivery in the background. Called after anything
// that changes what a volume should hold — a rotation, a retirement. Content hashing makes
// an unnecessary sweep cost one query and no Docker calls.
func (s *Server) resyncKeyringDeliveries(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	go func() {
		deliveries, err := s.store.AllKeyringDeliveries(ctx)
		if err != nil {
			return
		}
		for _, d := range deliveries {
			_ = s.reportKeyringDeliverySync(ctx, d)
		}
	}()
}

// reportKeyringDeliverySync syncs one delivery and records the outcome on it.
func (s *Server) reportKeyringDeliverySync(ctx context.Context, d *store.KeyringDelivery) error {
	hash, err := s.syncKeyringDelivery(ctx, d)
	if err == nil && hash == d.SyncedHash && d.Status == "ok" {
		return nil // nothing changed, nothing written, nothing to record
	}
	_ = s.store.MarkKeyringDeliverySynced(ctx, d.ID, hash, err)
	if err == nil {
		d.SyncedHash, d.Status, d.LastError = hash, "ok", ""
	} else {
		d.Status, d.LastError = "error", err.Error()
	}
	return err
}

// syncKeyringDelivery makes the volume on every node of the delivery's environment hold
// the desired files. Returns the content hash it delivered (or should have).
func (s *Server) syncKeyringDelivery(ctx context.Context, d *store.KeyringDelivery) (string, error) {
	files, hash, err := s.keyringFiles(ctx, d)
	if err != nil {
		return "", err
	}
	if hash == d.SyncedHash && d.Status == "ok" {
		return hash, nil // the volume already holds this
	}

	env, err := s.pool.Get(d.EnvID)
	if err != nil {
		return hash, fmt.Errorf("the environment is not connected")
	}

	// Key material is 0600 no matter what — there is no public half in this payload.
	vf := make([]volumes.File, 0, len(files))
	for name, content := range files {
		vf = append(vf, volumes.File{Name: name, Data: content, Mode: 0o600})
	}

	// Every node: a local volume exists per machine, and the consumer may be on any of
	// them (or move). Writing a few kilobytes to a node that never mounts it is cheaper
	// than a rule for where the consumer lives.
	var errs []string
	for _, node := range env.Nodes() {
		if err := volumes.Write(ctx, node, d.Volume, vf, d.UID, d.GID); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		restartTargets(ctx, node, d.RestartTargets)
	}
	if len(errs) > 0 {
		return hash, fmt.Errorf("delivering to %s: %s", d.Volume, strings.Join(errs, "; "))
	}
	return hash, nil
}

// keyringJSON is the delivered document — the contract applications code against.
// Marshalled with sorted map keys (encoding/json sorts them), so the same version set
// always produces the same bytes and the content hash is stable.
type keyringJSON struct {
	Keyring string                    `json:"keyring"`
	Current string                    `json:"current"`
	Keys    map[string]keyringJSONKey `json:"keys"`
}

type keyringJSONKey struct {
	Material  string `json:"material"` // base64, 32 bytes
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

// keyringFiles is the desired state: filename → content, and a hash over the lot.
func (s *Server) keyringFiles(ctx context.Context, d *store.KeyringDelivery) (map[string][]byte, string, error) {
	k, err := s.store.KeyringByID(ctx, d.KeyringID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", errors.New("the keyring this delivery carries no longer exists")
	}
	if err != nil {
		return nil, "", err
	}
	versions, err := s.store.KeyringVersions(ctx, d.KeyringID)
	if err != nil {
		return nil, "", err
	}

	doc := keyringJSON{Keyring: k.Name, Keys: map[string]keyringJSONKey{}}
	var currentRaw []byte
	for _, v := range versions {
		if v.State == store.KeyringVersionRetired {
			continue // stops appearing; this is what retirement IS at the volume
		}
		material, err := s.sealer.Open(v.MaterialEnc)
		if err != nil {
			return nil, "", errors.New("could not decrypt a keyring version (was the master key replaced?)")
		}
		doc.Keys[v.ID] = keyringJSONKey{
			Material: material, State: v.State,
			CreatedAt: v.CreatedAt.UTC().Format(time.RFC3339),
		}
		if v.State == store.KeyringVersionActive {
			doc.Current = v.ID
			if currentRaw, err = base64.StdEncoding.DecodeString(material); err != nil {
				return nil, "", errors.New("a keyring version's material is not base64 — the row was tampered with or corrupted")
			}
		}
	}
	if doc.Current == "" {
		return nil, "", errors.New("the keyring has no active version — rotate it first")
	}

	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, "", err
	}
	files := map[string][]byte{
		k.Name + ".json":        append(blob, '\n'),
		k.Name + ".current.key": currentRaw,
	}

	// Hash the desired state, not the tar: tar headers carry mtimes, and a hash that
	// changed with the clock would rewrite every volume on every sweep.
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(files[n]))
		h.Write(files[n])
	}
	fmt.Fprintf(h, "uid=%d gid=%d", d.UID, d.GID)
	return files, hex.EncodeToString(h.Sum(nil)), nil
}
