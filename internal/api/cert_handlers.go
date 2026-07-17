package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/certs"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// certName is what a certificate, CA or key may be called. Strict because a certificate's
// name becomes filenames inside a delivered volume (<name>.crt, <name>.key): no spaces, no
// slashes, no leading dot, nothing a shell or a YAML file would misread.
var certName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func badName(w http.ResponseWriter, r *http.Request) {
	httpx.BadRequest(w, r, "Names here become filenames: letters, digits, dots, dashes and underscores, starting with a letter or digit, 64 characters at most.")
}

// ── certificate authorities ─────────────────────────────────────────────────────

type caView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Subject   string     `json:"subject"`
	KeyAlgo   string     `json:"key_algo,omitempty"`
	CanSign   bool       `json:"can_sign"`
	NotBefore time.Time  `json:"not_before"`
	NotAfter  time.Time  `json:"not_after"`
	Status    string     `json:"status"`
	RotatesID string     `json:"rotates_id,omitempty"`
	Overlap   *time.Time `json:"overlap_until,omitempty"`
	WarnDays  int        `json:"warn_days"`
	InUse     int        `json:"in_use"`    // certificates this CA signed
	Protected bool       `json:"protected"` // part of the deployment; delete refused
}

func viewCA(ca *store.CertAuthority, inUse int) caView {
	v := caView{
		ID: ca.ID, Name: ca.Name, Subject: ca.Subject, KeyAlgo: ca.KeyAlgo,
		CanSign: ca.CanSign(), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		Status: ca.Status, RotatesID: ca.RotatesID, WarnDays: ca.WarnDays, InUse: inUse,
		Protected: ca.Protected,
	}
	if !ca.OverlapUntil.IsZero() {
		t := ca.OverlapUntil
		v.Overlap = &t
	}
	return v
}

func (s *Server) handleListCAs(w http.ResponseWriter, r *http.Request) {
	cas, err := s.store.ListCertAuthorities(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]caView, 0, len(cas))
	for _, ca := range cas {
		n, _ := s.store.CertAuthorityInUse(r.Context(), ca.ID)
		out = append(out, viewCA(ca, n))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type caRequest struct {
	Name string `json:"name"`

	// Create mode.
	CommonName string `json:"common_name"`
	Org        string `json:"org"`
	KeyAlgo    string `json:"key_algo"`
	Days       int    `json:"days"`

	// Upload mode. A cert without its key is accepted deliberately: it is a trust-only
	// anchor Daffa can bundle and deliver but never sign with.
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
}

func (s *Server) handleCreateCA(w http.ResponseWriter, r *http.Request) {
	var req caRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !certName.MatchString(req.Name) {
		badName(w, r)
		return
	}

	ca := &store.CertAuthority{Name: req.Name, Status: "active"}

	if strings.TrimSpace(req.CertPEM) != "" {
		// Upload. This is how the existing internal-ca.{crt,key} comes in.
		if err := certs.ValidateCAUpload(req.CertPEM, req.KeyPEM); err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_ca", err.Error())
			return
		}
		parsed, err := certs.ParseCert(req.CertPEM)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_ca", err.Error())
			return
		}
		ca.CertPEM = req.CertPEM
		ca.Subject = parsed.Subject.String()
		ca.KeyAlgo = certs.DescribeKey(parsed.PublicKey)
		ca.NotBefore, ca.NotAfter = parsed.NotBefore, parsed.NotAfter
		if req.KeyPEM != "" {
			sealed, err := s.sealer.Seal(req.KeyPEM)
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			ca.KeyEnc = sealed
		}
	} else {
		// Create. ECDSA P-256, ten years — the same shape internal-ca.sh rotates to,
		// with a modern key.
		algo := certs.KeyAlgo(req.KeyAlgo)
		if algo == "" {
			algo = certs.ECDSAP256
		}
		if req.Days <= 0 {
			req.Days = 3650
		}
		cn := strings.TrimSpace(req.CommonName)
		if cn == "" {
			httpx.BadRequest(w, r, "A common name is required — it is what the CA calls itself in every chain it signs.")
			return
		}
		certPEM, keyPEM, err := certs.CreateCA(cn, strings.TrimSpace(req.Org), algo, req.Days)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_ca", err.Error())
			return
		}
		parsed, _ := certs.ParseCert(certPEM)
		sealed, err := s.sealer.Seal(keyPEM)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		ca.CertPEM = certPEM
		ca.KeyEnc = sealed
		ca.Subject = parsed.Subject.String()
		ca.KeyAlgo = string(algo)
		ca.NotBefore, ca.NotAfter = parsed.NotBefore, parsed.NotAfter
	}

	if u, ok := auth.UserFrom(r.Context()); ok {
		ca.CreatedBy = u.ID
	}
	if err := s.store.CreateCertAuthority(r.Context(), ca); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "A certificate authority with that name already exists.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "ca.create", Target: ca.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"subject": ca.Subject, "can_sign": ca.CanSign(), "uploaded": req.CertPEM != ""}),
	})
	httpx.JSON(w, http.StatusOK, viewCA(ca, 0))
}

func (s *Server) handleDeleteCA(w http.ResponseWriter, r *http.Request) {
	ca, err := s.store.CertAuthorityByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_ca", "No such certificate authority.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// A CA the deployment provisioned for its own edge is off-limits from here, the same as
	// a system network or volume — deleting it would take the console's own TLS down.
	if ca.Protected {
		httpx.Fail(w, r, http.StatusBadRequest, "protected",
			"This certificate authority is part of the Daffa deployment and cannot be deleted from here.")
		return
	}

	// Refuse rather than orphan: a certificate whose CA vanished can never renew again,
	// and the day you find out is the day it expires.
	n, err := s.store.CertAuthorityInUse(r.Context(), ca.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n > 0 {
		httpx.Fail(w, r, http.StatusConflict, "in_use",
			"Certificates issued by this CA still exist. Delete or re-issue them first.")
		return
	}

	if err := s.store.DeleteCertAuthority(r.Context(), ca.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{Action: "ca.delete", Target: ca.Name, Outcome: "ok"})
	s.resyncDeliveries(r.Context()) // the trust bundle just changed
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// caRotateRequest stages a successor. Everything is defaultable: the CN mirrors the
// incumbent (rotation is not a rename), the name gets a -next suffix, and the overlap
// window defaults to 30 days.
type caRotateRequest struct {
	Name        string `json:"name"`
	CommonName  string `json:"common_name"`
	KeyAlgo     string `json:"key_algo"`
	Days        int    `json:"days"`
	OverlapDays int    `json:"overlap_days"`
}

// handleRotateCA is PHASE 1 of the two-phase rotation (docs/certs.md): stage a successor
// alongside the incumbent. Nothing is re-signed and nothing can break — the new root simply
// starts appearing in the trust bundle so distribution can begin.
func (s *Server) handleRotateCA(w http.ResponseWriter, r *http.Request) {
	ca, err := s.store.CertAuthorityByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_ca", "No such certificate authority.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if ca.Status != "active" {
		httpx.Fail(w, r, http.StatusConflict, "not_active", "Only an active CA can be rotated.")
		return
	}

	var req caRotateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	// One staged successor at a time — internal-ca.sh's "NEXT already staged" refusal.
	all, err := s.store.ListCertAuthorities(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	for _, other := range all {
		if other.Status == "next" && other.RotatesID == ca.ID {
			httpx.Fail(w, r, http.StatusConflict, "rotation_in_flight",
				"A successor for this CA is already staged. Activate or delete it first.")
			return
		}
	}

	// Defaults mirror the incumbent: same CN (rotation is not a rename), fresh ten years.
	cn := strings.TrimSpace(req.CommonName)
	org := ""
	if parsed, err := certs.ParseCert(ca.CertPEM); err == nil {
		if cn == "" {
			cn = parsed.Subject.CommonName
		}
		if len(parsed.Subject.Organization) > 0 {
			org = parsed.Subject.Organization[0]
		}
	}
	algo := certs.KeyAlgo(req.KeyAlgo)
	if algo == "" {
		algo = certs.ECDSAP256
	}
	if req.Days <= 0 {
		req.Days = 3650
	}
	if req.OverlapDays <= 0 {
		req.OverlapDays = 30
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = ca.Name + "-next"
	}
	if !certName.MatchString(name) {
		badName(w, r)
		return
	}

	certPEM, keyPEM, err := certs.CreateCA(cn, org, algo, req.Days)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "bad_ca", err.Error())
		return
	}
	sealed, err := s.sealer.Seal(keyPEM)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	parsed, _ := certs.ParseCert(certPEM)

	next := &store.CertAuthority{
		Name: name, Subject: parsed.Subject.String(), CertPEM: certPEM, KeyEnc: sealed,
		KeyAlgo: string(algo), NotBefore: parsed.NotBefore, NotAfter: parsed.NotAfter,
		Status: "next", RotatesID: ca.ID,
		OverlapUntil: time.Now().AddDate(0, 0, req.OverlapDays),
		WarnDays:     ca.WarnDays,
	}
	if u, ok := auth.UserFrom(r.Context()); ok {
		next.CreatedBy = u.ID
	}
	if err := s.store.CreateCertAuthority(r.Context(), next); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "A certificate authority with that name already exists.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "ca.rotate", Target: ca.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"next": next.Name, "overlap_days": req.OverlapDays}),
	})
	s.notifyCA(context.WithoutCancel(r.Context()), next,
		fmt.Sprintf("A successor for the CA “%s” is staged.", ca.Name),
		fmt.Sprintf("The trust bundle now carries both roots until %s. Install the new root everywhere that trusts the old one — operator machines, WARP profiles — then activate it. Leaves are untouched until then; nothing breaks by waiting.",
			next.OverlapUntil.Format("2006-01-02")))
	s.resyncDeliveries(r.Context()) // push the widened bundle out immediately
	httpx.JSON(w, http.StatusOK, viewCA(next, 0))
}

// caActivateRequest is the explicit confirmation activation demands — it is the step
// that breaks anything that never installed the new root.
type caActivateRequest struct {
	Confirm bool `json:"confirm"`
}

// handleActivateCA is PHASE 2: promote the staged successor and re-sign every leaf of the
// old root. This is the step that breaks anything that never installed the new root, which
// is why it demands an explicit confirmation and never fires on a timer.
func (s *Server) handleActivateCA(w http.ResponseWriter, r *http.Request) {
	next, err := s.store.CertAuthorityByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_ca", "No such certificate authority.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if next.Status != "next" {
		httpx.Fail(w, r, http.StatusConflict, "not_staged", "Only a staged (next) CA can be activated.")
		return
	}

	var req caActivateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if !req.Confirm {
		httpx.Fail(w, r, http.StatusBadRequest, "confirm_required",
			"Activating re-signs every certificate under the new root. Anything that has not installed it will stop trusting them the moment its consumer reloads. Confirm that the new root is distributed.")
		return
	}

	old, err := s.store.CertAuthorityByID(r.Context(), next.RotatesID)
	if errors.Is(err, store.ErrNotFound) {
		old = nil // the incumbent was deleted mid-rotation; promotion alone remains
	} else if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Re-sign first, promote after: if any leaf fails, the rotation stays staged and
	// nothing has been half-switched. Already re-signed leaves are fine either way —
	// during overlap both roots are trusted.
	if old != nil {
		if err := s.resignLeaves(r.Context(), old, next); err != nil {
			httpx.Fail(w, r, http.StatusBadGateway, "resign_failed", err.Error())
			return
		}
	}

	next.Status = "active"
	next.RotatesID = ""
	if err := s.store.UpdateCertAuthority(r.Context(), next); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if old != nil {
		old.Status = "retired"
		// The retired root stays in the trust bundle until the announced overlap window
		// ends, then falls out on its own.
		old.OverlapUntil = next.OverlapUntil
		if err := s.store.UpdateCertAuthority(r.Context(), old); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}

	target := next.Name
	if old != nil {
		target = old.Name + " -> " + next.Name
	}
	s.audit(r.Context(), store.AuditEntry{Action: "ca.activate", Target: target, Outcome: "ok"})
	s.resyncDeliveries(r.Context())
	httpx.JSON(w, http.StatusOK, viewCA(next, 0))
}

// resignLeaves moves every leaf of `from` onto `to`, reusing each leaf's key and SANs —
// internal-ca.sh's resign_leaves(), with the verify-before-install kept.
func (s *Server) resignLeaves(ctx context.Context, from, to *store.CertAuthority) error {
	toKey, err := s.sealer.Open(to.KeyEnc)
	if err != nil {
		return errors.New("could not decrypt the new CA's key (was the master key replaced?)")
	}
	leaves, err := s.store.CertificatesByCA(ctx, from.ID)
	if err != nil {
		return err
	}
	for _, c := range leaves {
		leafKey, err := s.sealer.Open(c.KeyEnc)
		if err != nil {
			return fmt.Errorf("could not decrypt the key for %s (was the master key replaced?)", c.Name)
		}
		renewed, err := certs.Renew(to.CertPEM, toKey, c.CertPEM, leafKey, c.ValidityDays)
		if err != nil {
			return fmt.Errorf("re-signing %s: %w", c.Name, err)
		}
		if err := certs.Verify(renewed, to.CertPEM); err != nil {
			return fmt.Errorf("re-signed %s does not verify — refusing to replace a working certificate: %w", c.Name, err)
		}
		parsed, _ := certs.ParseCert(renewed)
		c.CAID = to.ID
		c.CertPEM = renewed
		c.NotBefore, c.NotAfter = parsed.NotBefore, parsed.NotAfter
		c.Status, c.LastError = "ok", ""
		if err := s.store.UpdateCertificate(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// trustBundle is every root a client should currently trust: active and staged CAs always,
// retired ones until their announced overlap window has passed.
func (s *Server) trustBundle(ctx context.Context) (string, error) {
	cas, err := s.store.ListCertAuthorities(ctx)
	if err != nil {
		return "", err
	}
	var pems []string
	for _, ca := range cas {
		switch ca.Status {
		case "active", "next":
			pems = append(pems, ca.CertPEM)
		case "retired":
			if time.Now().Before(ca.OverlapUntil) {
				pems = append(pems, ca.CertPEM)
			}
		}
	}
	return certs.Bundle(pems...)
}

func (s *Server) handleTrustBundle(w http.ResponseWriter, r *http.Request) {
	bundle, err := s.trustBundle(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="ca-bundle.crt"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(bundle))
}

// ── certificates ────────────────────────────────────────────────────────────────

type certView struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	CAID            string    `json:"ca_id,omitempty"`
	CAName          string    `json:"ca_name,omitempty"`
	SANs            []string  `json:"sans"`
	KeyAlgo         string    `json:"key_algo,omitempty"`
	NotBefore       time.Time `json:"not_before"`
	NotAfter        time.Time `json:"not_after"`
	ValidityDays    int       `json:"validity_days"`
	RenewBeforeDays int       `json:"renew_before_days"`
	Status          string    `json:"status"`
	LastError       string    `json:"last_error,omitempty"`
	InUse           int       `json:"in_use"`    // deliveries carrying it
	Protected       bool      `json:"protected"` // part of the deployment; delete refused
}

func viewCert(c *store.Certificate, caName string, inUse int) certView {
	return certView{
		ID: c.ID, Name: c.Name, CAID: c.CAID, CAName: caName,
		SANs: strings.Fields(c.SANs), KeyAlgo: c.KeyAlgo,
		NotBefore: c.NotBefore, NotAfter: c.NotAfter,
		ValidityDays: c.ValidityDays, RenewBeforeDays: c.RenewBeforeDays,
		Status: c.Status, LastError: c.LastError, InUse: inUse,
		Protected: c.Protected,
	}
}

func (s *Server) handleListCertificates(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListCertificates(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	caName := map[string]string{}
	if cas, err := s.store.ListCertAuthorities(r.Context()); err == nil {
		for _, ca := range cas {
			caName[ca.ID] = ca.Name
		}
	}
	out := make([]certView, 0, len(list))
	for _, c := range list {
		n, _ := s.store.CertificateInUse(r.Context(), c.ID)
		out = append(out, viewCert(c, caName[c.CAID], n))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type certRequest struct {
	Name string `json:"name"`

	// Issue mode.
	CAID            string   `json:"ca_id"`
	SANs            []string `json:"sans"`
	KeyAlgo         string   `json:"key_algo"`
	ValidityDays    int      `json:"validity_days"`
	RenewBeforeDays int      `json:"renew_before_days"`

	// Upload mode.
	CertPEM  string `json:"cert_pem"`
	ChainPEM string `json:"chain_pem"`
	KeyPEM   string `json:"key_pem"`
}

func (s *Server) handleCreateCertificate(w http.ResponseWriter, r *http.Request) {
	var req certRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !certName.MatchString(req.Name) {
		badName(w, r)
		return
	}

	c := &store.Certificate{Name: req.Name, RenewBeforeDays: req.RenewBeforeDays}

	if strings.TrimSpace(req.CertPEM) != "" {
		// Upload: tracked, delivered, alerted on — but only its owner can renew it.
		if err := certs.ValidateLeafUpload(req.CertPEM, req.ChainPEM, req.KeyPEM); err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_certificate", err.Error())
			return
		}
		parsed, _ := certs.ParseCert(req.CertPEM)
		sealed, err := s.sealer.Seal(req.KeyPEM)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		c.CertPEM, c.ChainPEM, c.KeyEnc = req.CertPEM, req.ChainPEM, sealed
		c.SANs = strings.Join(certs.SANList(parsed), " ")
		c.KeyAlgo = certs.DescribeKey(parsed.PublicKey)
		c.NotBefore, c.NotAfter = parsed.NotBefore, parsed.NotAfter
		c.ValidityDays = int(parsed.NotAfter.Sub(parsed.NotBefore).Hours() / 24)
	} else {
		// Issue: one click instead of the openssl recipe in traefik/certs/README.md.
		ca, err := s.store.CertAuthorityByID(r.Context(), req.CAID)
		if errors.Is(err, store.ErrNotFound) {
			httpx.BadRequest(w, r, "Choose a certificate authority to issue from.")
			return
		}
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if !ca.CanSign() {
			httpx.Fail(w, r, http.StatusBadRequest, "trust_only",
				"That CA was uploaded without its private key, so Daffa cannot sign with it. Upload the key, or issue from a CA Daffa created.")
			return
		}
		sans := cleanSANs(req.SANs)
		if len(sans) == 0 {
			httpx.BadRequest(w, r, "At least one SAN is required — the hostnames (or IPs) this certificate will serve.")
			return
		}
		algo := certs.KeyAlgo(req.KeyAlgo)
		if algo == "" {
			algo = certs.ECDSAP256
		}
		if req.ValidityDays <= 0 {
			req.ValidityDays = 398
		}
		caKey, err := s.sealer.Open(ca.KeyEnc)
		if err != nil {
			httpx.Error(w, r, errors.New("could not decrypt the CA key (was the master key replaced?)"))
			return
		}
		certPEM, keyPEM, err := certs.Issue(ca.CertPEM, caKey, sans, algo, req.ValidityDays)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "issue_failed", err.Error())
			return
		}
		parsed, _ := certs.ParseCert(certPEM)
		sealed, err := s.sealer.Seal(keyPEM)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		c.CAID = ca.ID
		c.CertPEM, c.KeyEnc = certPEM, sealed
		c.SANs = strings.Join(sans, " ")
		c.KeyAlgo = string(algo)
		c.NotBefore, c.NotAfter = parsed.NotBefore, parsed.NotAfter
		c.ValidityDays = req.ValidityDays
	}

	if u, ok := auth.UserFrom(r.Context()); ok {
		c.CreatedBy = u.ID
	}
	if err := s.store.CreateCertificate(r.Context(), c); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "A certificate with that name already exists.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "cert.create", Target: c.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"sans": c.SANs, "issued": c.Issued()}),
	})
	httpx.JSON(w, http.StatusOK, viewCert(c, "", 0))
}

func (s *Server) certByPath(w http.ResponseWriter, r *http.Request) (*store.Certificate, bool) {
	c, err := s.store.CertificateByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_certificate", "No such certificate.")
		return nil, false
	}
	if err != nil {
		httpx.Error(w, r, err)
		return nil, false
	}
	return c, true
}

// handleUpdateCertificate edits what is editable. For an ISSUED certificate that includes
// its SANs — the edit re-issues immediately, with the same key, which is the fix for
// "adding a hostname means an openssl session". For an UPLOADED one it accepts a fresh
// cert_pem/key_pem pair: renewal by re-upload.
func (s *Server) handleUpdateCertificate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.certByPath(w, r)
	if !ok {
		return
	}
	var req certRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	// The name is immutable: it is the filename deliveries have already written into
	// volumes, and a rename would leave stale key material behind on every host.
	if req.Name != "" && req.Name != c.Name {
		httpx.Fail(w, r, http.StatusBadRequest, "immutable_name",
			"A certificate's name becomes filenames inside delivered volumes and cannot change. Create a new certificate instead.")
		return
	}
	if req.RenewBeforeDays > 0 {
		c.RenewBeforeDays = req.RenewBeforeDays
	}
	if req.ValidityDays > 0 && c.Issued() {
		c.ValidityDays = req.ValidityDays
	}

	detail := map[string]any{}

	if strings.TrimSpace(req.CertPEM) != "" {
		if c.Issued() {
			httpx.Fail(w, r, http.StatusBadRequest, "issued_certificate",
				"This certificate is issued by a Daffa CA — renew or re-issue it instead of uploading over it.")
			return
		}
		if err := certs.ValidateLeafUpload(req.CertPEM, req.ChainPEM, req.KeyPEM); err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_certificate", err.Error())
			return
		}
		parsed, _ := certs.ParseCert(req.CertPEM)
		sealed, err := s.sealer.Seal(req.KeyPEM)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		c.CertPEM, c.ChainPEM, c.KeyEnc = req.CertPEM, req.ChainPEM, sealed
		c.SANs = strings.Join(certs.SANList(parsed), " ")
		c.KeyAlgo = certs.DescribeKey(parsed.PublicKey)
		c.NotBefore, c.NotAfter = parsed.NotBefore, parsed.NotAfter
		c.Status, c.LastError = "ok", ""
		detail["reuploaded"] = true
	}

	if sans := cleanSANs(req.SANs); len(sans) > 0 && strings.Join(sans, " ") != c.SANs {
		if !c.Issued() {
			httpx.Fail(w, r, http.StatusBadRequest, "uploaded_certificate",
				"An uploaded certificate's SANs are facts about it, not settings. Upload a replacement instead.")
			return
		}
		ca, caKey, err := s.signingCA(r.Context(), c.CAID)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadGateway, "ca_unavailable", err.Error())
			return
		}
		leafKey, err := s.sealer.Open(c.KeyEnc)
		if err != nil {
			httpx.Error(w, r, errors.New("could not decrypt the certificate's key (was the master key replaced?)"))
			return
		}
		reissued, err := certs.Reissue(ca.CertPEM, caKey, leafKey, sans, c.ValidityDays)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "reissue_failed", err.Error())
			return
		}
		parsed, _ := certs.ParseCert(reissued)
		c.CertPEM = reissued
		c.SANs = strings.Join(sans, " ")
		c.NotBefore, c.NotAfter = parsed.NotBefore, parsed.NotAfter
		c.Status, c.LastError = "ok", ""
		detail["sans"] = c.SANs
	}

	if err := s.store.UpdateCertificate(r.Context(), c); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "cert.update", Target: c.Name, Outcome: "ok", Detail: store.AuditDetail(detail),
	})
	s.resyncDeliveries(r.Context())
	httpx.JSON(w, http.StatusOK, viewCert(c, "", 0))
}

// certRenewRequest asks for an early renewal; rotate_key also mints a fresh private key.
type certRenewRequest struct {
	RotateKey bool `json:"rotate_key"`
}

// handleRenewCertificate renews now, without waiting for the window. With rotate_key it
// also generates a fresh key — the deliberate version of what renewal deliberately avoids.
func (s *Server) handleRenewCertificate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.certByPath(w, r)
	if !ok {
		return
	}
	if !c.Issued() {
		httpx.Fail(w, r, http.StatusBadRequest, "uploaded_certificate",
			"Daffa did not issue this certificate and cannot renew it. Upload a replacement when you have one.")
		return
	}
	var req certRenewRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	ca, caKey, err := s.signingCA(r.Context(), c.CAID)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "ca_unavailable", err.Error())
		return
	}

	if req.RotateKey {
		certPEM, keyPEM, err := certs.Issue(ca.CertPEM, caKey, strings.Fields(c.SANs),
			certs.KeyAlgo(c.KeyAlgo), c.ValidityDays)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "issue_failed", err.Error())
			return
		}
		sealed, err := s.sealer.Seal(keyPEM)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		parsed, _ := certs.ParseCert(certPEM)
		c.CertPEM, c.KeyEnc = certPEM, sealed
		c.NotBefore, c.NotAfter = parsed.NotBefore, parsed.NotAfter
	} else {
		leafKey, err := s.sealer.Open(c.KeyEnc)
		if err != nil {
			httpx.Error(w, r, errors.New("could not decrypt the certificate's key (was the master key replaced?)"))
			return
		}
		renewed, err := certs.Renew(ca.CertPEM, caKey, c.CertPEM, leafKey, c.ValidityDays)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "renew_failed", err.Error())
			return
		}
		if err := certs.Verify(renewed, ca.CertPEM); err != nil {
			httpx.Fail(w, r, http.StatusBadGateway, "verify_failed", err.Error())
			return
		}
		parsed, _ := certs.ParseCert(renewed)
		c.CertPEM = renewed
		c.NotBefore, c.NotAfter = parsed.NotBefore, parsed.NotAfter
	}
	c.Status, c.LastError = "ok", ""

	if err := s.store.UpdateCertificate(r.Context(), c); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "cert.renew", Target: c.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"rotate_key": req.RotateKey, "not_after": c.NotAfter}),
	})
	s.resyncDeliveries(r.Context())
	httpx.JSON(w, http.StatusOK, viewCert(c, "", 0))
}

func (s *Server) handleDeleteCertificate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.certByPath(w, r)
	if !ok {
		return
	}
	if c.Protected {
		httpx.Fail(w, r, http.StatusBadRequest, "protected",
			"This certificate is part of the Daffa deployment and cannot be deleted from here.")
		return
	}
	n, err := s.store.CertificateInUse(r.Context(), c.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n > 0 {
		httpx.Fail(w, r, http.StatusConflict, "in_use",
			"Deliveries still carry this certificate. Delete them first.")
		return
	}
	if err := s.store.DeleteCertificate(r.Context(), c.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{Action: "cert.delete", Target: c.Name, Outcome: "ok"})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// signingCA loads a CA and unseals its key, refusing cleanly when it cannot sign.
func (s *Server) signingCA(ctx context.Context, caID string) (*store.CertAuthority, string, error) {
	ca, err := s.store.CertAuthorityByID(ctx, caID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", errors.New("the CA that issued this certificate no longer exists")
	}
	if err != nil {
		return nil, "", err
	}
	if !ca.CanSign() {
		return nil, "", errors.New("the CA that issued this certificate holds no private key")
	}
	key, err := s.sealer.Open(ca.KeyEnc)
	if err != nil {
		return nil, "", errors.New("could not decrypt the CA key (was the master key replaced?)")
	}
	return ca, key, nil
}

func cleanSANs(in []string) []string {
	var out []string
	for _, s := range in {
		for _, f := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// ── encryption keys ─────────────────────────────────────────────────────────────

type keyView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Recipient string    `json:"recipient"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	InUse     int       `json:"in_use"` // backup jobs encrypting to it
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListEncryptionKeys(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		n, _ := s.store.EncryptionKeyInUse(r.Context(), k.ID)
		out = append(out, keyView{
			ID: k.ID, Name: k.Name, Recipient: k.Recipient, Source: k.Source,
			CreatedAt: k.CreatedAt, InUse: n,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

type keyRequest struct {
	Name      string `json:"name"`
	Recipient string `json:"recipient"` // present ⇒ import; absent ⇒ generate
}

// createdKeyResponse is the create answer. identity_file is the ONE payload in Daffa
// that ever carries a private key, and only on generation: created in memory, returned
// here for the operator to download, and never stored anywhere.
type createdKeyResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Recipient    string `json:"recipient"`
	Source       string `json:"source"`
	IdentityFile string `json:"identity_file,omitempty"`
}

// handleCreateKey either GENERATES an age keypair or IMPORTS a public recipient.
//
// Generation is the one response in Daffa that carries a private key, and it does so
// exactly once: the identity is created in memory, returned in this response for the
// operator to download, and never stored — not in the database, not on disk, not in a log.
// The audit entry records that a key was generated, never the key.
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req keyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !certName.MatchString(req.Name) {
		badName(w, r)
		return
	}

	k := &store.EncryptionKey{Name: req.Name}
	var identityFile string

	if strings.TrimSpace(req.Recipient) != "" {
		rec, err := certs.ParseAgeRecipient(req.Recipient)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_recipient", err.Error())
			return
		}
		k.Recipient, k.Source = rec, "imported"
	} else {
		rec, file, err := certs.GenerateAgeKey(time.Now())
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		k.Recipient, k.Source = rec, "generated"
		identityFile = file
	}

	if u, ok := auth.UserFrom(r.Context()); ok {
		k.CreatedBy = u.ID
	}
	if err := s.store.CreateEncryptionKey(r.Context(), k); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "An encryption key with that name already exists.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "key." + k.Source, Target: k.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"recipient": k.Recipient}),
	})

	// identity_file: the only time this value exists outside the operator's machine.
	// Download it or lose it — the server keeps no copy, which is the point.
	httpx.JSON(w, http.StatusOK, createdKeyResponse{
		ID: k.ID, Name: k.Name, Recipient: k.Recipient, Source: k.Source,
		IdentityFile: identityFile,
	})
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.store.EncryptionKeyByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_key", "No such encryption key.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Refusing is not bureaucracy here: removing a recipient from a job silently narrows
	// who can restore its backups, and the day that matters is the worst possible day.
	n, err := s.store.EncryptionKeyInUse(r.Context(), k.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n > 0 {
		httpx.Fail(w, r, http.StatusConflict, "in_use",
			"Backup jobs still encrypt to this key. Point them at another key first.")
		return
	}

	if err := s.store.DeleteEncryptionKey(r.Context(), k.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{Action: "key.delete", Target: k.Name, Outcome: "ok"})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
