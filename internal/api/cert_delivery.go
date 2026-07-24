package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
	"github.com/Mnshahawy/daffa/internal/volumes"
)

// A delivery keeps certificate material current inside a named volume on a host, where a
// container mounts it read-only. The write mechanics live in internal/volumes (extracted
// from here): everything is a Docker API call, so it works identically against the local
// socket and through an agent tunnel. No exec, no long-lived agent, nothing new listening.
//
// A delivery is the Daffa-managed CONTENTS of that volume, not one certificate's delivery —
// which is what lets it share one Traefik dynamic directory with a git-sourced volume
// source. Traefik reads exactly one directory, and its file provider ignores anything that
// is not .toml/.yaml/.yml, so the PEMs and both manifests sit beside the config fragments
// unseen. See mixed-config-volumes.md.

type deliveryCertView struct {
	CertID    string `json:"cert_id"`
	CertName  string `json:"cert_name,omitempty"`
	IsDefault bool   `json:"is_default"`
}

type deliveryView struct {
	ID      string `json:"id"`
	EnvID   string `json:"env_id"`
	EnvName string `json:"env_name,omitempty"`
	// Empty = trust-bundle-only: the volume carries ca-bundle.crt and nothing else.
	Certs          []deliveryCertView `json:"certs"`
	Volume         string             `json:"volume"`
	UID            int                `json:"uid"`
	GID            int                `json:"gid"`
	Traefik        bool               `json:"traefik"`
	RestartTargets string             `json:"restart_targets,omitempty"`
	BundleCAs      []string           `json:"bundle_cas,omitempty"` // empty = every managed CA
	MountPath      string             `json:"mount_path"`           // where the CONSUMER mounts this volume
	Status         string             `json:"status"`
	LastError      string             `json:"last_error,omitempty"`
	SyncedAt       *time.Time         `json:"synced_at,omitempty"`
	Protected      bool               `json:"protected"` // part of the deployment; delete refused
}

func (s *Server) viewDelivery(ctx context.Context, d *store.CertDelivery) deliveryView {
	v := deliveryView{
		ID: d.ID, EnvID: d.EnvID, EnvName: s.envName(ctx, d.EnvID),
		Volume: d.Volume, UID: d.UID, GID: d.GID,
		Traefik: d.Traefik, RestartTargets: d.RestartTargets, MountPath: d.MountPath,
		BundleCAs: strings.Fields(d.BundleCAs),
		Certs:     make([]deliveryCertView, 0, len(d.Certs)),
		Status:    d.Status, LastError: d.LastError, Protected: d.Protected,
	}
	for _, dc := range d.Certs {
		cv := deliveryCertView{CertID: dc.CertID, IsDefault: dc.IsDefault}
		if c, err := s.store.CertificateByID(ctx, dc.CertID); err == nil {
			cv.CertName = c.Name
		}
		v.Certs = append(v.Certs, cv)
	}
	if !d.SyncedAt.IsZero() {
		t := d.SyncedAt
		v.SyncedAt = &t
	}
	return v
}

func (s *Server) handleListCertDeliveries(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.CertsView)
	list, err := s.store.ListCertDeliveries(r.Context(), global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]deliveryView, 0, len(list))
	for _, d := range list {
		out = append(out, s.viewDelivery(r.Context(), d))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type deliveryCertRequest struct {
	CertID string `json:"cert_id"`
	// At most one per delivery; none is legitimate (Traefik keeps its own self-signed
	// default for unmatched SNI, which is a visible state rather than a wrong guess).
	IsDefault bool `json:"is_default"`
}

type certDeliveryRequest struct {
	EnvID string `json:"env_id"`
	// Empty = trust-bundle-only: the volume carries ca-bundle.crt and nothing else.
	Certs  []deliveryCertRequest `json:"certs"`
	Volume string                `json:"volume"`
	// Where the CONSUMER mounts this volume; empty = store.DefaultCertMountPath. It is
	// declared rather than inferred because Traefik resolves the paths inside tls.yml in
	// its OWN filesystem, and Daffa cannot know where a compose file mounted the volume.
	MountPath string `json:"mount_path"`
	UID       int    `json:"uid"`
	GID       int    `json:"gid"`
	// Traefik also writes tls.yml, the file-provider fragment that hot-reloads on renewal.
	Traefik bool `json:"traefik"`
	// Space-separated container names bounced after each changed sync, for consumers
	// that cannot hot-reload.
	RestartTargets string `json:"restart_targets"`
	// Which roots this delivery's ca-bundle.crt carries, by CA id; empty = all of them.
	// Select incumbents, not staged successors — lineage rides along (see trustBundle).
	BundleCAs []string `json:"bundle_cas"`
}

func (s *Server) handleCreateCertDelivery(w http.ResponseWriter, r *http.Request) {
	var req certDeliveryRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	// No mayUseEnv here, deliberately: certs.edit is global-only, so whoever passed the
	// route guard holds it everywhere — there is no narrower grant to escape.
	if _, err := s.pool.Get(req.EnvID); err != nil {
		httpx.BadRequest(w, r, "That environment is not connected.")
		return
	}
	certs, code, msg := s.resolveDeliveryCerts(r.Context(), req.EnvID, req.Certs)
	if code != "" {
		httpx.Fail(w, r, http.StatusBadRequest, code, msg)
		return
	}
	if req.Volume != "" && !certName.MatchString(req.Volume) {
		badName(w, r)
		return
	}
	mountPath, err := cleanMountPath(req.MountPath)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "bad_mount_path", err.Error())
		return
	}

	// A bundle selection names real, selectable roots. Staged successors are refused:
	// selection is by lineage anchor, and the successor rides along on its own.
	for _, id := range req.BundleCAs {
		ca, err := s.store.CertAuthorityByID(r.Context(), id)
		if err != nil {
			httpx.BadRequest(w, r, "No such certificate authority in the bundle selection: "+id)
			return
		}
		if ca.Status == "next" {
			httpx.Fail(w, r, http.StatusBadRequest, "staged_ca",
				fmt.Sprintf("“%s” is a staged successor. Select the CA it replaces — the successor is bundled automatically while the rotation is in flight.", ca.Name))
			return
		}
	}

	d := &store.CertDelivery{
		EnvID: req.EnvID, Certs: certs, Volume: req.Volume, MountPath: mountPath,
		UID: req.UID, GID: req.GID, Traefik: req.Traefik,
		RestartTargets: strings.TrimSpace(req.RestartTargets),
		BundleCAs:      strings.Join(req.BundleCAs, " "),
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		d.CreatedBy = u.ID
	}
	if d.Traefik {
		if err := s.refuseSecondTraefikDelivery(r.Context(), d); err != nil {
			httpx.Fail(w, r, http.StatusConflict, "traefik_volume_taken", err.Error())
			return
		}
	}
	if err := s.store.CreateCertDelivery(r.Context(), d); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "cert.deliver", Target: d.Volume, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"certs": certIDs(d.Certs), "traefik": d.Traefik, "bundle_cas": d.BundleCAs}),
	})

	// First sync now, in the background — creating a delivery should not hang the request
	// on a volume write, but the operator should see it go green within seconds.
	go func(d store.CertDelivery) {
		ctx := context.WithoutCancel(r.Context())
		s.reportDeliverySync(ctx, &d)
	}(*d)

	httpx.JSON(w, http.StatusOK, s.viewDelivery(r.Context(), d))
}

// handleUpdateCertDelivery replaces a delivery's editable state. The environment and the
// volume are not editable: they are what the Traefik uniqueness rule and the consumer's
// mount are keyed on, so moving either is a new delivery, not an edit.
func (s *Server) handleUpdateCertDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.CertDeliveryByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_delivery", "No such delivery.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req certDeliveryRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	certs, code, msg := s.resolveDeliveryCerts(r.Context(), d.EnvID, req.Certs)
	if code != "" {
		httpx.Fail(w, r, http.StatusBadRequest, code, msg)
		return
	}
	mountPath, err := cleanMountPath(req.MountPath)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "bad_mount_path", err.Error())
		return
	}
	for _, id := range req.BundleCAs {
		ca, err := s.store.CertAuthorityByID(r.Context(), id)
		if err != nil {
			httpx.BadRequest(w, r, "No such certificate authority in the bundle selection: "+id)
			return
		}
		if ca.Status == "next" {
			httpx.Fail(w, r, http.StatusBadRequest, "staged_ca",
				fmt.Sprintf("“%s” is a staged successor. Select the CA it replaces — the successor is bundled automatically while the rotation is in flight.", ca.Name))
			return
		}
	}

	d.Certs, d.MountPath, d.UID, d.GID = certs, mountPath, req.UID, req.GID
	d.Traefik = req.Traefik
	d.RestartTargets = strings.TrimSpace(req.RestartTargets)
	d.BundleCAs = strings.Join(req.BundleCAs, " ")
	if d.Traefik {
		if err := s.refuseSecondTraefikDelivery(r.Context(), d); err != nil {
			httpx.Fail(w, r, http.StatusConflict, "traefik_volume_taken", err.Error())
			return
		}
	}
	if err := s.store.UpdateCertDelivery(r.Context(), d); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "cert.delivery.update", Target: d.Volume, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"certs": certIDs(d.Certs), "traefik": d.Traefik,
			"mount_path": d.MountPath, "bundle_cas": d.BundleCAs}),
	})

	// Reconcile in the background, like create: an edit should not hang the request on a
	// volume write, and a stale file set is exactly what the next sweep is for. Forced,
	// because an edit that only moves mount_path still has to rewrite tls.yml.
	go func(d store.CertDelivery) {
		ctx := context.WithoutCancel(r.Context())
		d.SyncedHash = ""
		s.reportDeliverySync(ctx, &d)
	}(*d)

	httpx.JSON(w, http.StatusOK, s.viewDelivery(r.Context(), d))
}

// resolveDeliveryCerts validates a requested certificate set and returns it in store shape.
// A non-empty code is an operator-facing refusal; the message names the fix.
func (s *Server) resolveDeliveryCerts(ctx context.Context, envID string, reqs []deliveryCertRequest) ([]store.DeliveryCert, string, string) {
	out := make([]store.DeliveryCert, 0, len(reqs))
	seen := map[string]bool{}
	byFilename := map[string]string{} // <name>.crt → cert id that claimed it
	defaults := 0
	for _, rc := range reqs {
		if rc.CertID == "" || seen[rc.CertID] {
			continue // an empty slot or a repeat of the same certificate is a no-op, not an error
		}
		seen[rc.CertID] = true
		c, err := s.store.CertificateByID(ctx, rc.CertID)
		if err != nil {
			return nil, "no_such_cert", "No such certificate: " + rc.CertID
		}
		// An env-scoped cert stays in its environment; only a SHARED cert goes anywhere.
		if c.EnvID != "" && c.EnvID != envID {
			return nil, "wrong_environment", fmt.Sprintf(
				"The certificate “%s” belongs to another environment. Deliver it there, or create one scoped to this environment.", c.Name)
		}
		// Names are unique per environment, but a SHARED certificate and an env-scoped one
		// may share a name — and they would then share a filename in the volume, one
		// silently overwriting the other. Refuse rather than invent a prefix: renaming
		// files inside volumes that already exist is a worse surprise.
		if other, taken := byFilename[c.Name]; taken {
			return nil, "name_collision", fmt.Sprintf(
				"Two selected certificates are both named “%s”, so both want %s.crt in the volume. Carry one of them, or rename it.",
				c.Name, c.Name) + " (" + other + " and " + c.ID + ")"
		}
		byFilename[c.Name] = c.ID
		if rc.IsDefault {
			defaults++
		}
		out = append(out, store.DeliveryCert{CertID: rc.CertID, IsDefault: rc.IsDefault})
	}
	if defaults > 1 {
		return nil, "multiple_defaults",
			"Only one certificate can be the default. Traefik has a single stores.default.defaultCertificate; the rest are matched by SNI."
	}
	return out, "", ""
}

// refuseSecondTraefikDelivery keeps one volume's tls.yml owned by one delivery. Two would
// each rewrite the other's fragment forever, and both would report ok — a delivery's
// synced_hash covers only its own desired state, so neither notices being overwritten.
func (s *Server) refuseSecondTraefikDelivery(ctx context.Context, d *store.CertDelivery) error {
	if !d.Traefik {
		return nil // no fragment, nothing to own: several plain deliveries share a volume freely
	}
	other, err := s.store.TraefikDeliveryForVolume(ctx, d.EnvID, d.Volume)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if other.ID == d.ID {
		return nil
	}
	return fmt.Errorf("another delivery already renders tls.yml into the volume %q on this environment. "+
		"Carry these certificates on that delivery, or give this one its own volume", d.Volume)
}

// refuseDeliveryOwnedNames is the one definition of "these filenames belong to the
// certificate delivery" — the fragment, the trust bundle, its manifest, and a .crt/.key
// pair per carried certificate. A volume with no Traefik delivery owns nothing and refuses
// nothing.
//
// Both writers of a shared dynamic directory ask this question, at different moments: the
// inline source asks when its files are SAVED (they are in the request, so the answer is
// free), and every source asks again when it SYNCS (a git subtree is only known after a
// clone, which is too expensive to do on every save). The pre-flight is the better error —
// it stops the operator before the bad file set is stored — and the sync check is the
// backstop that a repository edited elsewhere still cannot land.
func (s *Server) refuseDeliveryOwnedNames(ctx context.Context, envID, volume string, names []string) error {
	d, err := s.store.TraefikDeliveryForVolume(ctx, envID, volume)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	owned := map[string]bool{"tls.yml": true, "ca-bundle.crt": true, volumes.ManifestCerts: true}
	for _, dc := range d.Certs {
		c, err := s.store.CertificateByID(ctx, dc.CertID)
		if err != nil {
			continue // a cert that no longer exists owns no filename; the delivery will say so
		}
		owned[c.Name+".crt"] = true
		owned[c.Name+".key"] = true
	}
	for _, name := range names {
		if owned[name] {
			return fmt.Errorf("%s is written by the certificate delivery on this volume — "+
				"remove it here, or point this source at a different volume", name)
		}
	}
	return nil
}

// cleanMountPath validates where the consumer says it mounts the volume. The path is never
// opened here — it is interpolated into YAML that TRAEFIK resolves — so the check is about
// producing a fragment that means what the operator thinks it means.
func cleanMountPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return store.DefaultCertMountPath, nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", errors.New("the mount path must be absolute — it is where the consumer container mounts this volume")
	}
	clean := path.Clean(p)
	if clean == "/" {
		return "", errors.New("the mount path cannot be /")
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return "", errors.New("the mount path cannot contain ..")
		}
	}
	return clean, nil
}

func certIDs(certs []store.DeliveryCert) []string {
	out := make([]string, 0, len(certs))
	for _, c := range certs {
		out = append(out, c.CertID)
	}
	return out
}

func (s *Server) handleSyncCertDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.CertDeliveryByID(r.Context(), r.PathValue("id"))
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
	if err := s.reportDeliverySync(r.Context(), d); err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "sync_failed", err.Error())
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "cert.sync", Target: d.Volume, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, s.viewDelivery(r.Context(), d))
}

func (s *Server) handleDeleteCertDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.CertDeliveryByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_delivery", "No such delivery.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The delivery that keeps the console's own edge volume current is off-limits from here —
	// removing it would let that certificate go stale and, on renewal, silently stop updating.
	if d.Protected {
		httpx.Fail(w, r, http.StatusBadRequest, "protected",
			"This delivery is part of the Daffa deployment and cannot be deleted from here.")
		return
	}

	// The volume and its contents are left in place: the consumer may still be serving
	// with them, and Daffa deleting key material out from under a running proxy is a
	// worse surprise than a stale file. The volume is the operator's to remove.
	if err := s.store.DeleteCertDelivery(r.Context(), d.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "cert.undeliver", Target: d.Volume, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── the reconciler ──────────────────────────────────────────────────────────────

// resyncDeliveries sweeps every delivery in the background. Called after anything that
// changes what a volume should hold — a renewal, a rotation, a bundle change. Content
// hashing makes an unnecessary sweep cost one query and no Docker calls.
func (s *Server) resyncDeliveries(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	go func() {
		deliveries, err := s.store.AllCertDeliveries(ctx)
		if err != nil {
			return
		}
		for _, d := range deliveries {
			_ = s.reportDeliverySync(ctx, d)
		}
	}()
}

// reportDeliverySync syncs one delivery and records the outcome on it.
func (s *Server) reportDeliverySync(ctx context.Context, d *store.CertDelivery) error {
	hash, err := s.syncCertDelivery(ctx, d)
	if err == nil && hash == d.SyncedHash && d.Status == "ok" {
		return nil // nothing changed, nothing written, nothing to record
	}
	_ = s.store.MarkCertDeliverySynced(ctx, d.ID, hash, err)
	if err == nil {
		d.SyncedHash, d.Status, d.LastError = hash, "ok", ""
	} else {
		d.Status, d.LastError = "error", err.Error()
	}
	return err
}

// syncCertDelivery makes the volume on every node of the delivery's environment hold the
// desired files. Returns the content hash it delivered (or should have).
func (s *Server) syncCertDelivery(ctx context.Context, d *store.CertDelivery) (string, error) {
	files, hash, err := s.deliveryFiles(ctx, d)
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

	// Mode policy is cert delivery's, not the volume writer's: private keys are 0600 no
	// matter what, everything else is world-readable public material.
	vf := make([]volumes.File, 0, len(files))
	names := make([]string, 0, len(files))
	current := make(map[string]bool, len(files))
	for name, content := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, ".key") {
			mode = 0o600
		}
		vf = append(vf, volumes.File{Name: name, Data: content, Mode: mode})
		names = append(names, name)
		current[name] = true
	}
	sort.Strings(names)
	manifest := []volumes.File{{Name: volumes.ManifestCerts, Data: volumes.Manifest("", hash, names)}}

	// Every node: a local volume exists per machine, and the consumer may be on any of
	// them (or move). Writing a few kilobytes of PEM to a node that never mounts it is
	// cheaper than a rule for where Traefik lives.
	var errs []string
	for _, node := range env.Nodes() {
		// The delivery mirrors its own manifest, exactly as a volume source mirrors its
		// own: a certificate dropped from the delivery — or renamed, which changes both its
		// filenames — must stop existing in the volume, and a private key left behind after
		// the operator removed it is the worst kind of leftover. Bounded by the manifest, so
		// it can only ever delete files Daffa itself wrote: never the consumer's acme.json,
		// and never the middleware fragments a volume source writes beside it.
		var stale []string
		prev, err := volumes.ReadFile(ctx, node, d.Volume, volumes.ManifestCerts)
		switch {
		case err == nil:
			for _, name := range volumes.ParseManifest(prev) {
				if !current[name] {
					stale = append(stale, name)
				}
			}
		case errors.Is(err, volumes.ErrNotExist), errors.Is(err, volumes.ErrNoVolume):
			// First sync, or a volume that predates the manifest: plain overlay, deletes nothing.
		default:
			errs = append(errs, fmt.Sprintf("%s: reading the previous manifest: %v", node.Name, err))
			continue
		}

		// Order is load-bearing, and it is the volume-source order for the same reasons:
		// content first, stale removal second, the manifest LAST as the commit point — so a
		// failed removal cannot lose the stale list and report ok over an orphaned key.
		if err := volumes.Write(ctx, node, d.Volume, vf, d.UID, d.GID); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		if err := volumes.RemoveFiles(ctx, node, d.Volume, stale); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		if err := volumes.Write(ctx, node, d.Volume, manifest, d.UID, d.GID); err != nil {
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

// deliveryFiles is the desired state: filename → content, and a hash over the lot.
func (s *Server) deliveryFiles(ctx context.Context, d *store.CertDelivery) (map[string][]byte, string, error) {
	files := map[string][]byte{}

	bundle, err := s.trustBundle(ctx, strings.Fields(d.BundleCAs))
	if err != nil {
		return nil, "", err
	}
	if bundle != "" {
		files["ca-bundle.crt"] = []byte(bundle)
	}

	// Carried certificates, in the order the store returned them (by name), so the rendered
	// fragment — and therefore the hash — does not depend on row order.
	var defaultName string
	var carried []string
	for _, dc := range d.Certs {
		c, err := s.store.CertificateByID(ctx, dc.CertID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", errors.New("a certificate this delivery carries no longer exists")
		}
		if err != nil {
			return nil, "", err
		}
		// Belt and braces for the create-time check — env and cert are both immutable,
		// but a mismatch here means key material headed to the wrong environment.
		if c.EnvID != "" && c.EnvID != d.EnvID {
			return nil, "", fmt.Errorf("the certificate %q belongs to another environment", c.Name)
		}
		key, err := s.sealer.Open(c.KeyEnc)
		if err != nil {
			return nil, "", errors.New("could not decrypt a certificate's key (was the master key replaced?)")
		}
		// The .crt carries the full chain — a server that presents only the leaf works on
		// machines that have the intermediate cached and fails on the ones that matter.
		pem := c.CertPEM
		if c.ChainPEM != "" {
			pem += c.ChainPEM
		}
		files[c.Name+".crt"] = []byte(pem)
		files[c.Name+".key"] = []byte(key)
		carried = append(carried, c.Name)
		if dc.IsDefault {
			defaultName = c.Name
		}
	}
	if d.Traefik && len(carried) > 0 {
		files["tls.yml"] = []byte(traefikFragment(d.MountPath, carried, defaultName))
	}

	// Hash the desired state, not the tar: tar headers carry mtimes, and a hash that
	// changed with the clock would rewrite every volume on every sweep. mount_path rides
	// along inside tls.yml's bytes, so a moved mount is a changed hash for free.
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

// traefikFragment is the file-provider snippet that makes Traefik serve the delivered
// certificates — and hot-reload them on every renewal, which is what retires the
// `docker compose restart traefik` from internal-setup's renewal cron.
//
// mountPath is where the CONSUMER mounts the volume: Traefik resolves these paths in its
// own filesystem, and Daffa cannot know where an arbitrary compose file put the volume.
//
// With no default certificate the stores block is omitted entirely rather than guessed at.
// Traefik then keeps its own self-signed default for unmatched SNI — a visible, diagnosable
// state, where picking an arbitrary certificate would serve the wrong name to somebody and
// look like it worked.
func traefikFragment(mountPath string, names []string, defaultName string) string {
	var b strings.Builder
	b.WriteString("# Written by Daffa. Do not edit: it is rewritten on every renewal.\ntls:\n")
	if defaultName != "" {
		fmt.Fprintf(&b, `  stores:
    default:
      defaultCertificate:
        certFile: %[1]s/%[2]s.crt
        keyFile: %[1]s/%[2]s.key
`, mountPath, defaultName)
	}
	b.WriteString("  certificates:\n")
	for _, n := range names {
		fmt.Fprintf(&b, `    - certFile: %[1]s/%[2]s.crt
      keyFile: %[1]s/%[2]s.key
`, mountPath, n)
	}
	return b.String()
}

// restartTargets bounces the listed containers, for consumers that cannot hot-reload.
// A target that does not exist on this node is not an error — the delivery goes to every
// node, and the consumer runs on one of them.
func restartTargets(ctx context.Context, node *dockerx.Node, targets string) {
	for _, name := range strings.Fields(targets) {
		_ = node.Client.ContainerRestart(ctx, name, container.StopOptions{})
	}
}
