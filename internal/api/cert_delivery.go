package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
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
// mountPath is the convention the rendered Traefik fragment assumes: the consumer mounts
// the volume at this path. It has to be a convention, because the paths written inside
// tls.yml are resolved by TRAEFIK, and Daffa cannot know where an arbitrary compose file
// chose to mount the volume. docs/certs.md documents it; the delivery view repeats it.
const certMountPath = "/etc/traefik/dynamic-certs"

type deliveryView struct {
	ID             string     `json:"id"`
	EnvID          string     `json:"env_id"`
	EnvName        string     `json:"env_name,omitempty"`
	CertID         string     `json:"cert_id,omitempty"`
	CertName       string     `json:"cert_name,omitempty"` // empty = trust-bundle-only
	Volume         string     `json:"volume"`
	UID            int        `json:"uid"`
	GID            int        `json:"gid"`
	Traefik        bool       `json:"traefik"`
	RestartTargets string     `json:"restart_targets,omitempty"`
	BundleCAs      []string   `json:"bundle_cas,omitempty"` // empty = every managed CA
	MountPath      string     `json:"mount_path"`           // the convention, for the UI to show
	Status         string     `json:"status"`
	LastError      string     `json:"last_error,omitempty"`
	SyncedAt       *time.Time `json:"synced_at,omitempty"`
	Protected      bool       `json:"protected"` // part of the deployment; delete refused
}

func (s *Server) viewDelivery(ctx context.Context, d *store.CertDelivery) deliveryView {
	v := deliveryView{
		ID: d.ID, EnvID: d.EnvID, EnvName: s.envName(ctx, d.EnvID),
		CertID: d.CertID, Volume: d.Volume, UID: d.UID, GID: d.GID,
		Traefik: d.Traefik, RestartTargets: d.RestartTargets, MountPath: certMountPath,
		BundleCAs: strings.Fields(d.BundleCAs),
		Status:    d.Status, LastError: d.LastError, Protected: d.Protected,
	}
	if d.CertID != "" {
		if c, err := s.store.CertificateByID(ctx, d.CertID); err == nil {
			v.CertName = c.Name
		}
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

type certDeliveryRequest struct {
	EnvID string `json:"env_id"`
	// Empty = trust-bundle-only: the volume carries ca-bundle.crt and nothing else.
	CertID string `json:"cert_id"`
	Volume string `json:"volume"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
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
	if req.CertID != "" {
		c, err := s.store.CertificateByID(r.Context(), req.CertID)
		if err != nil {
			httpx.BadRequest(w, r, "No such certificate.")
			return
		}
		// An env-scoped cert stays in its environment; only a SHARED cert goes anywhere.
		if c.EnvID != "" && c.EnvID != req.EnvID {
			httpx.Fail(w, r, http.StatusBadRequest, "wrong_environment",
				fmt.Sprintf("The certificate “%s” belongs to another environment. Deliver it there, or create one scoped to this environment.", c.Name))
			return
		}
	}
	if req.Volume != "" && !certName.MatchString(req.Volume) {
		badName(w, r)
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
		EnvID: req.EnvID, CertID: req.CertID, Volume: req.Volume,
		UID: req.UID, GID: req.GID, Traefik: req.Traefik,
		RestartTargets: strings.TrimSpace(req.RestartTargets),
		BundleCAs:      strings.Join(req.BundleCAs, " "),
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		d.CreatedBy = u.ID
	}
	if err := s.store.CreateCertDelivery(r.Context(), d); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "cert.deliver", Target: d.Volume, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"cert_id": d.CertID, "traefik": d.Traefik, "bundle_cas": d.BundleCAs}),
	})

	// First sync now, in the background — creating a delivery should not hang the request
	// on a volume write, but the operator should see it go green within seconds.
	go func(d store.CertDelivery) {
		ctx := context.WithoutCancel(r.Context())
		s.reportDeliverySync(ctx, &d)
	}(*d)

	httpx.JSON(w, http.StatusOK, s.viewDelivery(r.Context(), d))
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
	for name, content := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, ".key") {
			mode = 0o600
		}
		vf = append(vf, volumes.File{Name: name, Data: content, Mode: mode})
	}

	// Every node: a local volume exists per machine, and the consumer may be on any of
	// them (or move). Writing a few kilobytes of PEM to a node that never mounts it is
	// cheaper than a rule for where Traefik lives.
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

	if d.CertID != "" {
		c, err := s.store.CertificateByID(ctx, d.CertID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", errors.New("the certificate this delivery carries no longer exists")
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
			return nil, "", errors.New("could not decrypt the certificate's key (was the master key replaced?)")
		}
		// The .crt carries the full chain — a server that presents only the leaf works on
		// machines that have the intermediate cached and fails on the ones that matter.
		pem := c.CertPEM
		if c.ChainPEM != "" {
			pem += c.ChainPEM
		}
		files[c.Name+".crt"] = []byte(pem)
		files[c.Name+".key"] = []byte(key)

		if d.Traefik {
			files["tls.yml"] = []byte(traefikFragment(c.Name))
		}
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

// traefikFragment is the file-provider snippet that makes Traefik serve the delivered
// certificate — and hot-reload it on every renewal, which is what retires the
// `docker compose restart traefik` from internal-setup's renewal cron. It assumes the
// consumer mounts the volume at certMountPath; see the constant.
func traefikFragment(name string) string {
	return fmt.Sprintf(`# Written by Daffa. Do not edit: it is rewritten on every renewal.
tls:
  stores:
    default:
      defaultCertificate:
        certFile: %[1]s/%[2]s.crt
        keyFile: %[1]s/%[2]s.key
  certificates:
    - certFile: %[1]s/%[2]s.crt
      keyFile: %[1]s/%[2]s.key
`, certMountPath, name)
}

// restartTargets bounces the listed containers, for consumers that cannot hot-reload.
// A target that does not exist on this node is not an error — the delivery goes to every
// node, and the consumer runs on one of them.
func restartTargets(ctx context.Context, node *dockerx.Node, targets string) {
	for _, name := range strings.Fields(targets) {
		_ = node.Client.ContainerRestart(ctx, name, container.StopOptions{})
	}
}
