package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
	"github.com/Mnshahawy/daffa/internal/volumes"
)

// A fleet delivery composes certificates from ANY environment — grouped into
// subdirectories, each with its own trust bundle — into one volume on the CONSUMER's
// environment. The worked example is Wali, a fleet console in one environment that dials
// many environments' control planes and must present each one ITS client identity and
// verify each one against ITS roots. See fleet-deliveries.md.
//
// It is deliberately separate from cert deliveries: that entity's contract is "material
// stays inside its own environment", every one of its refusals assumes it, and the fleet
// case is the sanctioned exception — behind its own capability (fleet.edit), so an
// administrator can grant certificate management without granting cross-environment
// composition, or the reverse.
//
// No Traefik fragment and no mount path here: a cross-environment client identity has no
// business being served by this environment's proxy, and without tls.yml nothing needs to
// know where the consumer mounts the volume.

// fleetSubdir is one slug-safe path level. No dots and no slashes, so a subdir can never
// collide with the flat files (*.crt, *.key, the manifest all carry dots), never traverse,
// and never look like a config file to an extension-watching consumer.
var fleetSubdir = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

type fleetCertView struct {
	CertID   string `json:"cert_id"`
	CertName string `json:"cert_name,omitempty"`
	// The SOURCE environment; empty = shared. Named openly: the delivery's whole point
	// is carrying material across environments, and a list that hid the sources would
	// lie about what the volume holds.
	EnvID   string `json:"env_id,omitempty"`
	EnvName string `json:"env_name,omitempty"`
}

type fleetGroupView struct {
	Subdir string `json:"subdir"` // "" = the volume root
	// Explicit root selection; empty = derived, and DerivedCAs says what derivation
	// currently resolves to, so the operator sees the effective trust either way.
	BundleCAs  []string        `json:"bundle_cas,omitempty"`
	DerivedCAs []string        `json:"derived_cas,omitempty"`
	Certs      []fleetCertView `json:"certs"`
}

type fleetDeliveryView struct {
	ID             string           `json:"id"`
	EnvID          string           `json:"env_id"` // the CONSUMER's environment
	EnvName        string           `json:"env_name,omitempty"`
	Volume         string           `json:"volume"`
	UID            int              `json:"uid"`
	GID            int              `json:"gid"`
	RestartTargets string           `json:"restart_targets,omitempty"`
	Groups         []fleetGroupView `json:"groups"`
	Status         string           `json:"status"`
	LastError      string           `json:"last_error,omitempty"`
	SyncedAt       *time.Time       `json:"synced_at,omitempty"`
}

func (s *Server) viewFleetDelivery(ctx context.Context, d *store.FleetDelivery) fleetDeliveryView {
	v := fleetDeliveryView{
		ID: d.ID, EnvID: d.EnvID, EnvName: s.envName(ctx, d.EnvID),
		Volume: d.Volume, UID: d.UID, GID: d.GID, RestartTargets: d.RestartTargets,
		Groups: make([]fleetGroupView, 0, len(d.Groups)),
		Status: d.Status, LastError: d.LastError,
	}
	for _, g := range d.Groups {
		gv := fleetGroupView{
			Subdir:    g.Subdir,
			BundleCAs: strings.Fields(g.BundleCAs),
			Certs:     make([]fleetCertView, 0, len(g.CertIDs)),
		}
		derived := map[string]bool{}
		for _, id := range g.CertIDs {
			cv := fleetCertView{CertID: id}
			if c, err := s.store.CertificateByID(ctx, id); err == nil {
				cv.CertName, cv.EnvID = c.Name, c.EnvID
				if c.EnvID != "" {
					cv.EnvName = s.envName(ctx, c.EnvID)
				}
				if c.CAID != "" {
					derived[c.CAID] = true
				}
			}
			gv.Certs = append(gv.Certs, cv)
		}
		if len(gv.BundleCAs) == 0 {
			gv.DerivedCAs = sortedKeys(derived)
		}
		v.Groups = append(v.Groups, gv)
	}
	if !d.SyncedAt.IsZero() {
		t := d.SyncedAt
		v.SyncedAt = &t
	}
	return v
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleListFleetDeliveries(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.FleetView)
	list, err := s.store.ListFleetDeliveries(r.Context(), global, envs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]fleetDeliveryView, 0, len(list))
	for _, d := range list {
		out = append(out, s.viewFleetDelivery(r.Context(), d))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type fleetGroupRequest struct {
	Subdir string `json:"subdir"` // "" = the volume root
	// Which roots this group's ca-bundle.crt carries, by CA id. Empty = DERIVED from the
	// group's certificates' issuing CAs — not "all managed CAs": a fleet volume aggregates
	// many trust domains, and the full bundle is every root precisely where trust is being
	// kept apart on purpose.
	BundleCAs []string `json:"bundle_cas"`
	Certs     []string `json:"certs"` // certificate ids, from any environment or shared
}

type fleetDeliveryRequest struct {
	EnvID          string              `json:"env_id"` // the CONSUMER's environment
	Volume         string              `json:"volume"`
	UID            int                 `json:"uid"`
	GID            int                 `json:"gid"`
	RestartTargets string              `json:"restart_targets"`
	Groups         []fleetGroupRequest `json:"groups"`
}

// resolveFleetGroups validates a requested group set and returns it in store shape.
// A non-empty code is an operator-facing refusal; the message names the fix.
func (s *Server) resolveFleetGroups(ctx context.Context, reqs []fleetGroupRequest) ([]store.FleetGroup, string, string) {
	if len(reqs) == 0 {
		return nil, "no_groups", "This delivery would carry nothing. Add a group, or delete the delivery instead of emptying it."
	}
	out := make([]store.FleetGroup, 0, len(reqs))
	subdirs := map[string]bool{}
	placed := map[string]string{} // cert id → subdir that claimed it
	for _, rg := range reqs {
		if rg.Subdir != "" && !fleetSubdir.MatchString(rg.Subdir) {
			return nil, "bad_subdir", fmt.Sprintf(
				"%q is not a valid subdirectory: lowercase letters, digits and hyphens, up to 64 characters, one level.", rg.Subdir)
		}
		if subdirs[rg.Subdir] {
			return nil, "duplicate_subdir", fmt.Sprintf(
				"Two groups share the subdirectory %q. Merge them — one directory has one content list.", displaySubdir(rg.Subdir))
		}
		subdirs[rg.Subdir] = true

		g := store.FleetGroup{Subdir: rg.Subdir, BundleCAs: strings.Join(rg.BundleCAs, " ")}

		// A bundle selection names real, selectable roots — the cert-delivery rule:
		// select the incumbent, and a staged successor rides along by lineage.
		for _, id := range rg.BundleCAs {
			ca, err := s.store.CertAuthorityByID(ctx, id)
			if err != nil {
				return nil, "no_such_ca", "No such certificate authority in the bundle selection: " + id
			}
			if ca.Status == "next" {
				return nil, "staged_ca", fmt.Sprintf(
					"“%s” is a staged successor. Select the CA it replaces — the successor is bundled automatically while the rotation is in flight.", ca.Name)
			}
		}

		byFilename := map[string]string{} // <name> → cert id that claimed it, within this group
		seen := map[string]bool{}
		for _, certID := range rg.Certs {
			if certID == "" || seen[certID] {
				continue // an empty slot or a repeat within the group is a no-op, not an error
			}
			seen[certID] = true
			if other, ok := placed[certID]; ok {
				return nil, "cert_in_two_groups", fmt.Sprintf(
					"A certificate is selected in both %q and %q. It lands in exactly one place — pick one.",
					displaySubdir(other), displaySubdir(rg.Subdir))
			}
			placed[certID] = rg.Subdir
			c, err := s.store.CertificateByID(ctx, certID)
			if err != nil {
				return nil, "no_such_cert", "No such certificate: " + certID
			}
			// Any environment is allowed — that is the feature — but two same-named
			// certificates in one group would fight over one filename. The subdir IS the
			// disambiguator; the fix is to give each source its own.
			if other, taken := byFilename[c.Name]; taken {
				return nil, "name_collision", fmt.Sprintf(
					"Two certificates in %q are both named “%s”, so both want %s.crt there. Put each source environment in its own subdirectory.",
					displaySubdir(rg.Subdir), c.Name, c.Name) + " (" + other + " and " + c.ID + ")"
			}
			byFilename[c.Name] = c.ID
			g.CertIDs = append(g.CertIDs, certID)
		}
		out = append(out, g)
	}
	return out, "", ""
}

func displaySubdir(subdir string) string {
	if subdir == "" {
		return "the volume root"
	}
	return subdir
}

// refuseFleetVolumeClash is the volume-exclusivity rule, fleet side: a fleet delivery and
// a certificate delivery each mirror their OWN manifest, so two of them pruning one volume
// would eventually delete each other's files — and both would report ok, because a synced
// hash covers only its own desired state. One volume, one delivery. (Volume SOURCES may
// still share: they mirror a third manifest, and the filename clash is refused instead —
// see refuseDeliveryOwnedNames.)
func (s *Server) refuseFleetVolumeClash(ctx context.Context, d *store.FleetDelivery) error {
	others, err := s.store.DeliveriesForVolume(ctx, d.EnvID, d.Volume)
	if err != nil {
		return err
	}
	if len(others) > 0 {
		return fmt.Errorf("a certificate delivery already writes the volume %q on this environment — give this fleet delivery its own volume", d.Volume)
	}
	other, err := s.store.FleetDeliveryForVolume(ctx, d.EnvID, d.Volume)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if other.ID != d.ID {
		return fmt.Errorf("another fleet delivery already writes the volume %q on this environment — carry these groups on that delivery, or give this one its own volume", d.Volume)
	}
	return nil
}

func (s *Server) handleCreateFleetDelivery(w http.ResponseWriter, r *http.Request) {
	var req fleetDeliveryRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	// No mayUseEnv here, deliberately — the cert-delivery reasoning: fleet.edit is
	// global-only, so whoever passed the route guard holds it everywhere; there is no
	// narrower grant to escape.
	if _, err := s.pool.Get(req.EnvID); err != nil {
		httpx.BadRequest(w, r, "That environment is not connected.")
		return
	}
	if req.Volume != "" && !certName.MatchString(req.Volume) {
		badName(w, r)
		return
	}
	groups, code, msg := s.resolveFleetGroups(r.Context(), req.Groups)
	if code != "" {
		httpx.Fail(w, r, http.StatusBadRequest, code, msg)
		return
	}

	d := &store.FleetDelivery{
		EnvID: req.EnvID, Volume: req.Volume, UID: req.UID, GID: req.GID,
		RestartTargets: strings.TrimSpace(req.RestartTargets), Groups: groups,
	}
	if d.Volume == "" {
		d.Volume = "daffa-fleet-certs"
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		d.CreatedBy = u.ID
	}
	if err := s.refuseFleetVolumeClash(r.Context(), d); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "volume_taken", err.Error())
		return
	}
	if err := s.store.CreateFleetDelivery(r.Context(), d); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "fleet.deliver", Target: d.Volume, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"groups": auditFleetGroups(r.Context(), s, d)}),
	})

	// First sync now, in the background — creating a delivery should not hang the request
	// on a volume write, but the operator should see it go green within seconds.
	go func(d store.FleetDelivery) {
		ctx := context.WithoutCancel(r.Context())
		s.reportFleetDeliverySync(ctx, &d)
	}(*d)

	httpx.JSON(w, http.StatusOK, s.viewFleetDelivery(r.Context(), d))
}

// handleUpdateFleetDelivery replaces a delivery's editable state. The environment and the
// volume are not editable: they are what the consumer's mount is keyed on, so moving
// either is a new delivery, not an edit.
func (s *Server) handleUpdateFleetDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.FleetDeliveryByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_delivery", "No such fleet delivery.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req fleetDeliveryRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	groups, code, msg := s.resolveFleetGroups(r.Context(), req.Groups)
	if code != "" {
		httpx.Fail(w, r, http.StatusBadRequest, code, msg)
		return
	}

	d.UID, d.GID = req.UID, req.GID
	d.RestartTargets = strings.TrimSpace(req.RestartTargets)
	d.Groups = groups
	if err := s.store.UpdateFleetDelivery(r.Context(), d); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "fleet.delivery.update", Target: d.Volume, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"groups": auditFleetGroups(r.Context(), s, d)}),
	})

	// Reconcile in the background, like create: an edit should not hang the request on a
	// volume write, and a stale file set is exactly what the next sweep is for. Forced,
	// because a group whose selection changed may render the same filenames.
	go func(d store.FleetDelivery) {
		ctx := context.WithoutCancel(r.Context())
		d.SyncedHash = ""
		s.reportFleetDeliverySync(ctx, &d)
	}(*d)

	httpx.JSON(w, http.StatusOK, s.viewFleetDelivery(r.Context(), d))
}

func (s *Server) handleSyncFleetDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.FleetDeliveryByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_delivery", "No such fleet delivery.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Synchronous, and forced: "sync now" is the button an operator presses while looking
	// at a red status, and it should answer with the outcome, not with "started".
	d.SyncedHash = ""
	if err := s.reportFleetDeliverySync(r.Context(), d); err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "sync_failed", err.Error())
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "fleet.sync", Target: d.Volume, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, s.viewFleetDelivery(r.Context(), d))
}

func (s *Server) handleDeleteFleetDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.FleetDeliveryByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_delivery", "No such fleet delivery.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The volume and its contents are left in place — the cert-delivery stance: the
	// consumer may still be running on them, and Daffa deleting key material out from
	// under it is a worse surprise than a stale file. The volume is the operator's to remove.
	if err := s.store.DeleteFleetDelivery(r.Context(), d.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: d.EnvID, Action: "fleet.undeliver", Target: d.Volume, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// auditFleetGroups is the audit detail: each group's certificates WITH their source
// environments. Cross-environment key material moves on these requests, and the audit
// trail must say from where to where — that is the accepted-risk contract.
func auditFleetGroups(ctx context.Context, s *Server, d *store.FleetDelivery) []map[string]any {
	out := make([]map[string]any, 0, len(d.Groups))
	for _, g := range d.Groups {
		certs := make([]string, 0, len(g.CertIDs))
		for _, id := range g.CertIDs {
			entry := id
			if c, err := s.store.CertificateByID(ctx, id); err == nil {
				src := c.EnvID
				if src == "" {
					src = "shared"
				}
				entry = c.Name + " (" + s.envNameOr(ctx, src) + ")"
			}
			certs = append(certs, entry)
		}
		out = append(out, map[string]any{"subdir": g.Subdir, "bundle_cas": g.BundleCAs, "certs": certs})
	}
	return out
}

func (s *Server) envNameOr(ctx context.Context, envID string) string {
	if envID == "shared" {
		return "shared"
	}
	if n := s.envName(ctx, envID); n != "" {
		return n
	}
	return envID
}

// ── the reconciler ──────────────────────────────────────────────────────────────

// reportFleetDeliverySync syncs one fleet delivery and records the outcome on it.
func (s *Server) reportFleetDeliverySync(ctx context.Context, d *store.FleetDelivery) error {
	hash, err := s.syncFleetDelivery(ctx, d)
	if err == nil && hash == d.SyncedHash && d.Status == "ok" {
		return nil // nothing changed, nothing written, nothing to record
	}
	_ = s.store.MarkFleetDeliverySynced(ctx, d.ID, hash, err)
	if err == nil {
		d.SyncedHash, d.Status, d.LastError = hash, "ok", ""
	} else {
		d.Status, d.LastError = "error", err.Error()
	}
	return err
}

// syncFleetDelivery makes the volume on every node of the CONSUMER's environment hold the
// desired files. Returns the content hash it delivered (or should have). The mechanics are
// syncCertDelivery's exactly — the same writer, the same manifest-last commit point — with
// one difference: the manifest is ManifestFleet, so a fleet delivery can only ever prune
// files a fleet delivery wrote.
func (s *Server) syncFleetDelivery(ctx context.Context, d *store.FleetDelivery) (string, error) {
	files, hash, err := s.fleetDeliveryFiles(ctx, d)
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

	// Mode policy is the cert-delivery one: private keys are 0600 no matter what,
	// everything else is world-readable public material.
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
	manifest := []volumes.File{{Name: volumes.ManifestFleet, Data: volumes.Manifest("", hash, names)}}

	// Every node: a local volume exists per machine, and the consumer may be on any of
	// them (or move).
	var errs []string
	for _, node := range env.Nodes() {
		// Mirror our own manifest: a certificate dropped from a group — or a whole group
		// removed — must stop existing in the volume; a private key left behind after the
		// operator removed it is the worst kind of leftover. Bounded by the manifest, so
		// only files a fleet delivery wrote are ever deleted.
		var stale []string
		prev, err := volumes.ReadFile(ctx, node, d.Volume, volumes.ManifestFleet)
		switch {
		case err == nil:
			for _, name := range volumes.ParseManifest(prev) {
				if !current[name] {
					stale = append(stale, name)
				}
			}
		case errors.Is(err, volumes.ErrNotExist), errors.Is(err, volumes.ErrNoVolume):
			// First sync: plain overlay, deletes nothing.
		default:
			errs = append(errs, fmt.Sprintf("%s: reading the previous manifest: %v", node.Name, err))
			continue
		}

		// Order is load-bearing, and it is the cert-delivery order for the same reasons:
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

// fleetDeliveryFiles is the desired state: filename → content, and a hash over the lot.
// Group order (by subdir) and cert order (by name) come from the store, so the hash does
// not depend on how rows happen to come back.
func (s *Server) fleetDeliveryFiles(ctx context.Context, d *store.FleetDelivery) (map[string][]byte, string, error) {
	files := map[string][]byte{}

	for _, g := range d.Groups {
		prefix := ""
		if g.Subdir != "" {
			// Re-checked at sync time, belt and braces: a subdir that slipped past the
			// handler could write outside its own level.
			if !fleetSubdir.MatchString(g.Subdir) {
				return nil, "", fmt.Errorf("the subdirectory %q is not valid", g.Subdir)
			}
			prefix = g.Subdir + "/"
		}

		byFilename := map[string]bool{}
		derived := map[string]bool{}
		for _, certID := range g.CertIDs {
			c, err := s.store.CertificateByID(ctx, certID)
			if errors.Is(err, store.ErrNotFound) {
				return nil, "", errors.New("a certificate this delivery carries no longer exists")
			}
			if err != nil {
				return nil, "", err
			}
			if byFilename[c.Name] {
				return nil, "", fmt.Errorf("two certificates in %s are both named %q", displaySubdir(g.Subdir), c.Name)
			}
			byFilename[c.Name] = true
			key, err := s.sealer.Open(c.KeyEnc)
			if err != nil {
				return nil, "", errors.New("could not decrypt a certificate's key (was the master key replaced?)")
			}
			// The .crt carries the full chain — a client that presents only the leaf works
			// against peers that have the intermediate cached and fails against the ones
			// that matter.
			pem := c.CertPEM
			if c.ChainPEM != "" {
				pem += c.ChainPEM
			}
			files[prefix+c.Name+".crt"] = []byte(pem)
			files[prefix+c.Name+".key"] = []byte(key)
			if c.CAID != "" {
				derived[c.CAID] = true
			}
		}

		// The group's trust: the explicit selection, or the issuers of what it carries.
		// Both flow through trustBundle, so rotation overlap behaves identically to a
		// selected cert-delivery bundle. A group with no selection and only UPLOADED
		// certificates gets no bundle at all: Daffa cannot know what anchors an external
		// issuer implies, and guessing would silently teach a consumer to trust roots its
		// source never meant. Absence is visible; a wrong bundle looks like it worked.
		selected := strings.Fields(g.BundleCAs)
		if len(selected) == 0 {
			selected = sortedKeys(derived)
		}
		if len(selected) > 0 {
			bundle, err := s.trustBundle(ctx, selected)
			if err != nil {
				return nil, "", err
			}
			if bundle != "" {
				files[prefix+"ca-bundle.crt"] = []byte(bundle)
			}
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

// fleetOwnedNames is every filename the fleet delivery on a volume writes — what a volume
// source sharing the volume must not touch. Best-effort on the certificate names: an entry
// whose certificate vanished owns nothing, and the delivery's own status will say so.
func (s *Server) fleetOwnedNames(ctx context.Context, envID, volume string) (map[string]bool, error) {
	d, err := s.store.FleetDeliveryForVolume(ctx, envID, volume)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{volumes.ManifestFleet: true}
	for _, g := range d.Groups {
		prefix := ""
		if g.Subdir != "" {
			prefix = g.Subdir + "/"
		}
		owned[prefix+"ca-bundle.crt"] = true
		for _, id := range g.CertIDs {
			c, err := s.store.CertificateByID(ctx, id)
			if err != nil {
				continue
			}
			owned[prefix+c.Name+".crt"] = true
			owned[prefix+c.Name+".key"] = true
		}
	}
	return owned, nil
}
