package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Mnshahawy/daffa/internal/certs"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The console's own edge TLS is issued from an internal CA when the domain is not
// reachable by a public ACME challenge (a private hostname, split-horizon DNS, an
// air-gapped network). This is the same machinery an operator would drive by hand in the
// UI — an internal CA, a leaf certificate, a Traefik delivery — packaged so the installer
// can stand it up in one step and hand back a trust bundle.
//
// Fixed, regex-safe names (the domain rides in the SANs, not the row name) so a long or
// awkward hostname cannot produce an unusable certificate name, and so a re-run finds what
// the last run made instead of piling up duplicates.
const (
	edgeCAName       = "daffa-edge-ca"
	edgeCACommonName = "Daffa Edge CA"
	edgeCertName     = "daffa-edge"
)

// EdgeCertOptions parameterises BootstrapEdgeCert.
type EdgeCertOptions struct {
	Domain   string   // the hostname the certificate serves; becomes the first SAN
	SANs     []string // additional SANs (e.g. a bare apex alongside www)
	EnvID    string   // the environment to deliver into — the local host, normally
	Volume   string   // the volume Traefik reads dynamic certs from
	CADays   int      // CA validity; 0 → 10 years
	CertDays int      // leaf validity; 0 → ~13 months
}

// EdgeCertResult reports what BootstrapEdgeCert did.
type EdgeCertResult struct {
	CAID           string
	CertID         string
	DeliveryID     string
	Domain         string
	Volume         string
	TrustBundlePEM string // the CA(s) to install on client machines
}

// BootstrapEdgeCert provisions the console's own edge TLS from an internal CA: it creates
// (or reuses) a protected CA, issues (or reuses) a protected certificate for the domain,
// delivers it into the edge volume as a Traefik file-provider fragment, syncs it now, and
// returns the CA trust bundle. It is idempotent — a second run with the same domain and
// volume reuses what exists and just re-syncs.
//
// Everything it creates is marked Protected, so none of it can be deleted from the UI:
// removing the console's own certificate, CA, or delivery is not a mistake a click should
// be able to make. See the delete handlers in cert_handlers.go / cert_delivery.go.
func (s *Server) BootstrapEdgeCert(ctx context.Context, opts EdgeCertOptions) (EdgeCertResult, error) {
	var res EdgeCertResult
	domain := strings.ToLower(strings.TrimSpace(opts.Domain))
	if domain == "" {
		return res, errors.New("edge: a domain is required")
	}
	if strings.TrimSpace(opts.Volume) == "" {
		return res, errors.New("edge: a volume is required")
	}
	if _, err := s.pool.Get(opts.EnvID); err != nil {
		return res, fmt.Errorf("edge: environment is not connected: %w", err)
	}
	res.Domain, res.Volume = domain, opts.Volume

	ca, err := s.ensureEdgeCA(ctx, opts.CADays)
	if err != nil {
		return res, err
	}
	res.CAID = ca.ID

	cert, err := s.ensureEdgeCert(ctx, ca, domain, opts)
	if err != nil {
		return res, err
	}
	res.CertID = cert.ID

	d, err := s.ensureEdgeDelivery(ctx, opts.EnvID, opts.Volume, cert.ID)
	if err != nil {
		return res, err
	}
	res.DeliveryID = d.ID

	// Write the material into the volume now. The delivery worker would also reach it on
	// its next sweep, but the installer wants the certificate live before it returns.
	d.SyncedHash = "" // force, even if a prior run already synced identical content
	if err := s.reportDeliverySync(ctx, d); err != nil {
		return res, fmt.Errorf("edge: delivering the certificate into volume %q: %w", opts.Volume, err)
	}

	bundle, err := s.trustBundle(ctx, nil)
	if err != nil {
		return res, err
	}
	res.TrustBundlePEM = bundle
	return res, nil
}

func (s *Server) ensureEdgeCA(ctx context.Context, caDays int) (*store.CertAuthority, error) {
	cas, err := s.store.ListCertAuthorities(ctx)
	if err != nil {
		return nil, err
	}
	for _, ca := range cas {
		if ca.Name == edgeCAName && ca.CanSign() {
			return ca, nil
		}
	}

	if caDays <= 0 {
		caDays = 3650
	}
	certPEM, keyPEM, err := certs.CreateCA(edgeCACommonName, "Daffa", certs.ECDSAP256, caDays)
	if err != nil {
		return nil, fmt.Errorf("edge: creating CA: %w", err)
	}
	parsed, _ := certs.ParseCert(certPEM)
	sealed, err := s.sealer.Seal(keyPEM)
	if err != nil {
		return nil, err
	}
	ca := &store.CertAuthority{
		Name: edgeCAName, CertPEM: certPEM, KeyEnc: sealed,
		Subject: parsed.Subject.String(), KeyAlgo: string(certs.ECDSAP256),
		NotBefore: parsed.NotBefore, NotAfter: parsed.NotAfter,
		Status: "active", Protected: true, OutboundTrust: true,
	}
	if err := s.store.CreateCertAuthority(ctx, ca); err != nil {
		return nil, fmt.Errorf("edge: storing CA: %w", err)
	}
	return ca, nil
}

func (s *Server) ensureEdgeCert(ctx context.Context, ca *store.CertAuthority, domain string, opts EdgeCertOptions) (*store.Certificate, error) {
	list, err := s.store.ListCertificates(ctx, true, nil)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		if c.Name == edgeCertName {
			return c, nil
		}
	}

	sans := cleanSANs(append([]string{domain}, opts.SANs...))
	days := opts.CertDays
	if days <= 0 {
		days = 397
	}
	caKey, err := s.sealer.Open(ca.KeyEnc)
	if err != nil {
		return nil, errors.New("edge: could not decrypt the CA key (was the master key replaced?)")
	}
	certPEM, keyPEM, err := certs.Issue(ca.CertPEM, caKey, sans, certs.ECDSAP256, days, certs.UsageServer)
	if err != nil {
		return nil, fmt.Errorf("edge: issuing certificate: %w", err)
	}
	parsed, _ := certs.ParseCert(certPEM)
	sealed, err := s.sealer.Seal(keyPEM)
	if err != nil {
		return nil, err
	}
	c := &store.Certificate{
		Name: edgeCertName, CAID: ca.ID, SANs: strings.Join(sans, " "),
		KeyAlgo: string(certs.ECDSAP256), CertPEM: certPEM, KeyEnc: sealed,
		NotBefore: parsed.NotBefore, NotAfter: parsed.NotAfter, ValidityDays: days,
		Protected: true,
	}
	if err := s.store.CreateCertificate(ctx, c); err != nil {
		return nil, fmt.Errorf("edge: storing certificate: %w", err)
	}
	return c, nil
}

func (s *Server) ensureEdgeDelivery(ctx context.Context, envID, volume, certID string) (*store.CertDelivery, error) {
	all, err := s.store.AllCertDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.EnvID == envID && d.Volume == volume {
			return d, nil
		}
	}
	d := &store.CertDelivery{
		EnvID: envID, Volume: volume, Traefik: true, Protected: true,
		Certs: []store.DeliveryCert{{CertID: certID, IsDefault: true}},
	}
	if err := s.store.CreateCertDelivery(ctx, d); err != nil {
		return nil, fmt.Errorf("edge: creating delivery: %w", err)
	}
	return d, nil
}
