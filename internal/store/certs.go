package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CertAuthority is an internal CA — either created by Daffa or imported. The certificate
// is public material and stays plaintext; the private key is sealed, and a CA imported
// without its key (KeyEnc empty) is a trust-only anchor: it can be bundled and delivered,
// but never signs.
type CertAuthority struct {
	ID        string
	Name      string
	Subject   string
	CertPEM   string
	KeyEnc    string
	KeyAlgo   string
	NotBefore time.Time
	NotAfter  time.Time
	// Status is the rotation state machine: active (signing), next (staged successor,
	// distribution in progress), retired (superseded; kept in the trust bundle until the
	// overlap window ends). See docs/certs.md.
	Status string
	// RotatesID, on a NEXT CA, names the active CA it will replace when activated.
	RotatesID    string
	OverlapUntil time.Time // zero unless a rotation is in flight
	WarnDays     int
	CreatedAt    time.Time
	CreatedBy    string
	// Protected marks a CA the deployment depends on (its own edge CA). Deletion is
	// refused while set — see the delete handler. Set only by `daffa edge init`.
	Protected bool
	// OutboundTrust: whether Daffa's OWN outbound TLS (registry reach-out, git clones)
	// trusts this CA beyond the system roots. Off for a CA that exists only to be
	// bundled into deliveries — someone else's trust anchor should not widen what the
	// console itself accepts.
	OutboundTrust bool
}

// CanSign reports whether Daffa holds this CA's private key.
func (ca *CertAuthority) CanSign() bool { return ca.KeyEnc != "" }

const caCols = `id, name, subject, cert_pem, key_enc, key_algo, not_before, not_after,
    status, rotates_id, overlap_until, warn_days, created_at, created_by, protected, outbound_trust`

func scanCA(sc interface{ Scan(...any) error }) (*CertAuthority, error) {
	var ca CertAuthority
	var notBefore, notAfter, createdAt string
	var rotatesID, overlapUntil, createdBy sql.NullString
	var protected, outboundTrust int
	err := sc.Scan(&ca.ID, &ca.Name, &ca.Subject, &ca.CertPEM, &ca.KeyEnc, &ca.KeyAlgo,
		&notBefore, &notAfter, &ca.Status, &rotatesID, &overlapUntil, &ca.WarnDays, &createdAt, &createdBy, &protected, &outboundTrust)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ca.NotBefore = parseTS(notBefore)
	ca.NotAfter = parseTS(notAfter)
	ca.RotatesID = rotatesID.String
	if overlapUntil.Valid {
		ca.OverlapUntil = parseTS(overlapUntil.String)
	}
	ca.CreatedAt = parseTS(createdAt)
	ca.CreatedBy = createdBy.String
	ca.Protected = protected != 0
	ca.OutboundTrust = outboundTrust != 0
	return &ca, nil
}

func (s *Store) CreateCertAuthority(ctx context.Context, ca *CertAuthority) error {
	if ca.ID == "" {
		ca.ID = "ca_" + NewID()
	}
	if ca.Status == "" {
		ca.Status = "active"
	}
	if ca.WarnDays <= 0 {
		ca.WarnDays = 180
	}
	ca.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO cert_authorities (`+caCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ca.ID, ca.Name, ca.Subject, ca.CertPEM, ca.KeyEnc, ca.KeyAlgo,
		ts(ca.NotBefore), ts(ca.NotAfter), ca.Status, nullStr(ca.RotatesID),
		nullTS(ca.OverlapUntil), ca.WarnDays, ts(ca.CreatedAt), nullStr(ca.CreatedBy),
		boolInt(ca.Protected), boolInt(ca.OutboundTrust))
	if err != nil {
		return fmt.Errorf("store: creating certificate authority: %w", err)
	}
	return nil
}

func (s *Store) CertAuthorityByID(ctx context.Context, id string) (*CertAuthority, error) {
	return scanCA(s.queryRow(ctx, `SELECT `+caCols+` FROM cert_authorities WHERE id = ?`, id))
}

func (s *Store) ListCertAuthorities(ctx context.Context) ([]*CertAuthority, error) {
	rows, err := s.query(ctx, `SELECT `+caCols+` FROM cert_authorities ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing certificate authorities: %w", err)
	}
	defer rows.Close()
	var out []*CertAuthority
	for rows.Next() {
		ca, err := scanCA(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ca)
	}
	return out, rows.Err()
}

// UpdateCertAuthority rewrites the mutable parts: rotation state and the certificate
// itself (which changes when a staged CA is promoted). Name and key material do not
// change under an existing id — a new CA is a new row.
func (s *Store) UpdateCertAuthority(ctx context.Context, ca *CertAuthority) error {
	_, err := s.exec(ctx, `UPDATE cert_authorities SET subject = ?, cert_pem = ?, key_enc = ?,
        key_algo = ?, not_before = ?, not_after = ?, status = ?, rotates_id = ?,
        overlap_until = ?, warn_days = ?, outbound_trust = ? WHERE id = ?`,
		ca.Subject, ca.CertPEM, ca.KeyEnc, ca.KeyAlgo, ts(ca.NotBefore), ts(ca.NotAfter),
		ca.Status, nullStr(ca.RotatesID), nullTS(ca.OverlapUntil), ca.WarnDays,
		boolInt(ca.OutboundTrust), ca.ID)
	return err
}

// CertAuthorityInUse counts live certificates issued by this CA plus deliveries that
// select it into their bundle, so deletion can be refused instead of orphaning either —
// a CA deleted out of a selection would silently shrink what its consumers trust.
func (s *Store) CertAuthorityInUse(ctx context.Context, id string) (int, error) {
	var leaves, selected int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM certificates WHERE ca_id = ?`, id).Scan(&leaves); err != nil {
		return 0, err
	}
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM cert_deliveries
        WHERE (' ' || bundle_cas || ' ') LIKE ('% ' || ? || ' %')`, id).Scan(&selected)
	return leaves + selected, err
}

func (s *Store) DeleteCertAuthority(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM cert_authorities WHERE id = ?`, id)
	return err
}

// ── certificates ────────────────────────────────────────────────────────────────

// Certificate is a leaf: issued by one of Daffa's CAs (CAID set — renewable) or uploaded
// (CAID empty — tracked and delivered, but only its owner can renew it, by uploading again).
type Certificate struct {
	ID   string
	Name string
	// EnvID scopes the certificate to one environment; empty = SHARED, visible and
	// deliverable everywhere. Immutable after create, like Name and for the same
	// reason: deliveries have already written this cert's files into volumes chosen
	// under its visibility.
	EnvID    string
	CAID     string
	SANs     string // space-separated; first entry is the CN
	KeyAlgo  string
	CertPEM  string
	ChainPEM string
	KeyEnc   string
	// Usages: 'server', 'client' or 'server client' — the EKUs every (re-)signing of
	// this leaf carries. The ROW is the source of truth so a renewal cannot silently
	// drop clientAuth; for uploaded certs it is derived from the PEM, display-only.
	Usages string

	NotBefore       time.Time
	NotAfter        time.Time
	ValidityDays    int
	RenewBeforeDays int

	Status    string // ok | error
	LastError string
	CreatedAt time.Time
	CreatedBy string
	// Protected marks the deployment's own edge certificate; deletion is refused while set.
	Protected bool
}

// Issued reports whether Daffa can renew this certificate itself.
func (c *Certificate) Issued() bool { return c.CAID != "" }

const certCols = `id, name, env_id, ca_id, sans, key_algo, cert_pem, chain_pem, key_enc,
    not_before, not_after, validity_days, renew_before_days, status, last_error,
    created_at, created_by, protected, usages`

func scanCert(sc interface{ Scan(...any) error }) (*Certificate, error) {
	var c Certificate
	var envID, caID, createdBy sql.NullString
	var notBefore, notAfter, createdAt string
	var protected int
	err := sc.Scan(&c.ID, &c.Name, &envID, &caID, &c.SANs, &c.KeyAlgo, &c.CertPEM, &c.ChainPEM,
		&c.KeyEnc, &notBefore, &notAfter, &c.ValidityDays, &c.RenewBeforeDays,
		&c.Status, &c.LastError, &createdAt, &createdBy, &protected, &c.Usages)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.EnvID = envID.String
	c.CAID = caID.String
	c.NotBefore = parseTS(notBefore)
	c.NotAfter = parseTS(notAfter)
	c.CreatedAt = parseTS(createdAt)
	c.CreatedBy = createdBy.String
	c.Protected = protected != 0
	return &c, nil
}

func (s *Store) CreateCertificate(ctx context.Context, c *Certificate) error {
	if c.ID == "" {
		c.ID = "crt_" + NewID()
	}
	if c.Status == "" {
		c.Status = "ok"
	}
	if c.RenewBeforeDays <= 0 {
		c.RenewBeforeDays = 30
	}
	if c.Usages == "" {
		c.Usages = "server"
	}
	c.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO certificates (`+certCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, nullStr(c.EnvID), nullStr(c.CAID), c.SANs, c.KeyAlgo, c.CertPEM, c.ChainPEM, c.KeyEnc,
		ts(c.NotBefore), ts(c.NotAfter), c.ValidityDays, c.RenewBeforeDays,
		c.Status, c.LastError, ts(c.CreatedAt), nullStr(c.CreatedBy), boolInt(c.Protected), c.Usages)
	if err != nil {
		return fmt.Errorf("store: creating certificate: %w", err)
	}
	return nil
}

func (s *Store) CertificateByID(ctx context.Context, id string) (*Certificate, error) {
	return scanCert(s.queryRow(ctx, `SELECT `+certCols+` FROM certificates WHERE id = ?`, id))
}

// ListCertificates returns the certificates the caller may see: SHARED ones (no env)
// always, env-scoped ones on the caller's envs. Filters, never gates, like every List.
func (s *Store) ListCertificates(ctx context.Context, global bool, envs []string) ([]*Certificate, error) {
	where, args := "", []any(nil)
	if !global {
		in, inArgs := envIn(false, envs)
		if in == neverMatches {
			where = " WHERE env_id IS NULL"
		} else {
			// envIn's WHERE, widened: a NULL env is shared and visible to everyone.
			where = " WHERE (env_id IS NULL OR" + strings.TrimPrefix(in, " WHERE") + ")"
			args = inArgs
		}
	}
	rows, err := s.query(ctx, `SELECT `+certCols+` FROM certificates`+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing certificates: %w", err)
	}
	defer rows.Close()
	var out []*Certificate
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CertificatesByCA returns the leaves a CA has signed — the set a rotation must re-sign.
func (s *Store) CertificatesByCA(ctx context.Context, caID string) ([]*Certificate, error) {
	rows, err := s.query(ctx, `SELECT `+certCols+` FROM certificates WHERE ca_id = ? ORDER BY name`, caID)
	if err != nil {
		return nil, fmt.Errorf("store: listing certificates by CA: %w", err)
	}
	defer rows.Close()
	var out []*Certificate
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCertificate rewrites the mutable parts. env_id is deliberately absent: a
// certificate's environment, like its name, is fixed at create.
func (s *Store) UpdateCertificate(ctx context.Context, c *Certificate) error {
	_, err := s.exec(ctx, `UPDATE certificates SET name = ?, ca_id = ?, sans = ?, key_algo = ?,
        cert_pem = ?, chain_pem = ?, key_enc = ?, not_before = ?, not_after = ?,
        validity_days = ?, renew_before_days = ?, status = ?, last_error = ?, usages = ? WHERE id = ?`,
		c.Name, nullStr(c.CAID), c.SANs, c.KeyAlgo, c.CertPEM, c.ChainPEM, c.KeyEnc,
		ts(c.NotBefore), ts(c.NotAfter), c.ValidityDays, c.RenewBeforeDays,
		c.Status, c.LastError, c.Usages, c.ID)
	return err
}

// CertificateInUse counts deliveries carrying this cert, so deletion can be refused while
// something still consumes it.
func (s *Store) CertificateInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM cert_delivery_certs WHERE cert_id = ?`, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteCertificate(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM certificates WHERE id = ?`, id)
	return err
}

// ── deliveries ──────────────────────────────────────────────────────────────────

// DeliveryCert is one certificate a delivery carries. IsDefault marks the one that becomes
// tls.yml's stores.default.defaultCertificate — at most one per delivery, and none is a
// legitimate state (Traefik then falls back to its own self-signed default, which is
// visible and diagnosable; guessing a default would serve the wrong name to somebody).
type DeliveryCert struct {
	CertID    string
	IsDefault bool
}

// CertDelivery is the Daffa-managed CONTENTS of a named volume on one environment: the
// certificates it carries, the trust bundle, and — for Traefik — the file-provider
// fragment. The reconciler compares SyncedHash against the desired content and rewrites
// the volume when they differ.
//
// It is deliberately not "one certificate's delivery": a volume is the unit Traefik reads
// and the unit a git-sourced volume source shares, so it has to be the unit Daffa manages.
// See mixed-config-volumes.md.
type CertDelivery struct {
	ID    string
	EnvID string
	// Certs is empty for a trust-bundle-only delivery: the volume carries ca-bundle.crt
	// and nothing else.
	Certs  []DeliveryCert
	Volume string
	// MountPath is where the CONSUMER mounts this volume. Declared, not inferred, because
	// the paths written into tls.yml are resolved by Traefik in Traefik's filesystem, and
	// Daffa cannot know where an arbitrary compose file chose to mount the volume.
	MountPath string
	UID       int
	GID       int
	// Traefik also renders a file-provider tls.yml into the volume, so Traefik
	// hot-reloads instead of restarting.
	Traefik        bool
	RestartTargets string // space-separated container names; empty = consumer hot-reloads
	// BundleCAs selects which roots this delivery's ca-bundle.crt carries, as
	// space-separated CA ids; empty = every managed CA. Lineage rides along: a staged
	// successor joins via rotates_id, activation rewrites the selection to the promoted
	// id, and a retired root stays through its overlap window.
	BundleCAs string

	SyncedHash string
	SyncedAt   time.Time
	Status     string // pending | ok | error
	LastError  string
	CreatedAt  time.Time
	CreatedBy  string
	// Protected marks the delivery that keeps the deployment's own edge volume current;
	// deletion is refused while set.
	Protected bool
}

const deliveryCols = `id, env_id, volume, uid, gid, traefik, restart_targets,
    synced_hash, synced_at, status, last_error, created_at, created_by, protected, bundle_cas,
    mount_path`

func scanDelivery(sc interface{ Scan(...any) error }) (*CertDelivery, error) {
	var d CertDelivery
	var syncedAt, createdBy sql.NullString
	var createdAt string
	var traefik, protected int
	err := sc.Scan(&d.ID, &d.EnvID, &d.Volume, &d.UID, &d.GID, &traefik,
		&d.RestartTargets, &d.SyncedHash, &syncedAt, &d.Status, &d.LastError,
		&createdAt, &createdBy, &protected, &d.BundleCAs, &d.MountPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Traefik = traefik != 0
	if syncedAt.Valid {
		d.SyncedAt = parseTS(syncedAt.String)
	}
	d.CreatedAt = parseTS(createdAt)
	d.CreatedBy = createdBy.String
	d.Protected = protected != 0
	return &d, nil
}

// DefaultCertMountPath is where a delivery's volume is assumed to be mounted unless the
// operator says otherwise — the path every delivery used before mount_path existed, so an
// upgraded row keeps rendering exactly the tls.yml it rendered yesterday.
const DefaultCertMountPath = "/etc/traefik/dynamic-certs"

func (s *Store) CreateCertDelivery(ctx context.Context, d *CertDelivery) error {
	if d.ID == "" {
		d.ID = "dlv_" + NewID()
	}
	if d.Volume == "" {
		d.Volume = "daffa-certs"
	}
	if d.MountPath == "" {
		d.MountPath = DefaultCertMountPath
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	d.CreatedAt = now()

	// The row and its certificates go in together, or neither does: a committed delivery
	// whose certs failed to attach is a delivery that would reconcile to trust-bundle-only
	// and quietly remove the PEMs a proxy is serving.
	defer s.lockWrites()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: creating certificate delivery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO cert_deliveries (`+deliveryCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		d.ID, d.EnvID, d.Volume, d.UID, d.GID, boolInt(d.Traefik),
		d.RestartTargets, d.SyncedHash, nullTS(d.SyncedAt), d.Status, d.LastError,
		ts(d.CreatedAt), nullStr(d.CreatedBy), boolInt(d.Protected), d.BundleCAs,
		d.MountPath); err != nil {
		return fmt.Errorf("store: creating certificate delivery: %w", err)
	}
	if err := setDeliveryCertsTx(ctx, s, tx, d.ID, d.Certs); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateCertDelivery replaces the editable state of a delivery — what it carries, where it
// lands, and how it renders. env_id and the volume are not editable: both are what the
// uniqueness rule and the consumer's mount are keyed on, and moving either is a new
// delivery, not an edit.
func (s *Store) UpdateCertDelivery(ctx context.Context, d *CertDelivery) error {
	if d.MountPath == "" {
		d.MountPath = DefaultCertMountPath
	}
	defer s.lockWrites()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: updating certificate delivery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE cert_deliveries SET uid = ?, gid = ?,
        traefik = ?, restart_targets = ?, bundle_cas = ?, mount_path = ? WHERE id = ?`),
		d.UID, d.GID, boolInt(d.Traefik), d.RestartTargets, d.BundleCAs, d.MountPath,
		d.ID); err != nil {
		return fmt.Errorf("store: updating certificate delivery: %w", err)
	}
	if err := setDeliveryCertsTx(ctx, s, tx, d.ID, d.Certs); err != nil {
		return err
	}
	return tx.Commit()
}

func setDeliveryCertsTx(ctx context.Context, s *Store, tx *sql.Tx, deliveryID string, certs []DeliveryCert) error {
	if _, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM cert_delivery_certs WHERE delivery_id = ?`), deliveryID); err != nil {
		return fmt.Errorf("store: clearing delivery certificates: %w", err)
	}
	for _, c := range certs {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO cert_delivery_certs (delivery_id, cert_id, is_default) VALUES (?, ?, ?)`),
			deliveryID, c.CertID, boolInt(c.IsDefault)); err != nil {
			return fmt.Errorf("store: adding certificate %s to the delivery: %w", c.CertID, err)
		}
	}
	return nil
}

// attachDeliveryCerts loads the carried certificates for a batch of deliveries in one
// query, not one per delivery. Ordered by certificate NAME, because that order becomes the
// order of entries in the rendered tls.yml — and therefore part of the content hash, which
// must not depend on how rows happen to come back.
func (s *Store) attachDeliveryCerts(ctx context.Context, deliveries []*CertDelivery) error {
	if len(deliveries) == 0 {
		return nil
	}
	byID := make(map[string]*CertDelivery, len(deliveries))
	for _, d := range deliveries {
		byID[d.ID] = d
	}
	rows, err := s.query(ctx, `SELECT dc.delivery_id, dc.cert_id, dc.is_default
        FROM cert_delivery_certs dc
        JOIN certificates c ON c.id = dc.cert_id ORDER BY c.name`)
	if err != nil {
		return fmt.Errorf("store: loading delivery certificates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var deliveryID, certID string
		var isDefault int
		if err := rows.Scan(&deliveryID, &certID, &isDefault); err != nil {
			return err
		}
		if d, ok := byID[deliveryID]; ok {
			d.Certs = append(d.Certs, DeliveryCert{CertID: certID, IsDefault: isDefault != 0})
		}
	}
	return rows.Err()
}

func (s *Store) CertDeliveryByID(ctx context.Context, id string) (*CertDelivery, error) {
	d, err := scanDelivery(s.queryRow(ctx, `SELECT `+deliveryCols+` FROM cert_deliveries WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.attachDeliveryCerts(ctx, []*CertDelivery{d}); err != nil {
		return nil, err
	}
	return d, nil
}

// TraefikDeliveryForVolume finds the delivery that owns tls.yml in a volume, if any. It is
// how the two writers of a shared dynamic directory find each other: a volume source's sync
// asks before it writes, so a repo that carries its own tls.yml is refused rather than left
// to fight the delivery forever. ErrNotFound means the volume has no Traefik delivery.
func (s *Store) TraefikDeliveryForVolume(ctx context.Context, envID, volume string) (*CertDelivery, error) {
	d, err := scanDelivery(s.queryRow(ctx, `SELECT `+deliveryCols+` FROM cert_deliveries
        WHERE env_id = ? AND volume = ? AND traefik = 1`, envID, volume))
	if err != nil {
		return nil, err
	}
	if err := s.attachDeliveryCerts(ctx, []*CertDelivery{d}); err != nil {
		return nil, err
	}
	return d, nil
}

// DeliveriesForVolume returns every delivery writing into one volume in one environment —
// what the deploy hook reconciles after syncing a stack's volume sources, so a stack never
// comes up against git config that is present and certificates that are not.
func (s *Store) DeliveriesForVolume(ctx context.Context, envID, volume string) ([]*CertDelivery, error) {
	rows, err := s.query(ctx, `SELECT `+deliveryCols+` FROM cert_deliveries
        WHERE env_id = ? AND volume = ? ORDER BY created_at`, envID, volume)
	if err != nil {
		return nil, fmt.Errorf("store: listing deliveries for a volume: %w", err)
	}
	defer rows.Close()
	var out []*CertDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachDeliveryCerts(ctx, out)
}

// ListCertDeliveries returns the deliveries on hosts the caller may see.
func (s *Store) ListCertDeliveries(ctx context.Context, global bool, envs []string) ([]*CertDelivery, error) {
	where, args := envIn(global, envs)
	if where == neverMatches {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT `+deliveryCols+` FROM cert_deliveries`+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing certificate deliveries: %w", err)
	}
	defer rows.Close()
	var out []*CertDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachDeliveryCerts(ctx, out)
}

// AllCertDeliveries returns every delivery, ignoring permissions. It is for the
// reconciler, which runs on behalf of the system and not of any user.
func (s *Store) AllCertDeliveries(ctx context.Context) ([]*CertDelivery, error) {
	return s.ListCertDeliveries(ctx, true, nil)
}

// MarkCertDeliverySynced records a reconcile outcome — the hash on success, the error
// verbatim on failure.
func (s *Store) MarkCertDeliverySynced(ctx context.Context, id, hash string, syncErr error) error {
	status, errText := "ok", ""
	if syncErr != nil {
		status, errText = "error", syncErr.Error()
	}
	_, err := s.exec(ctx, `UPDATE cert_deliveries SET synced_hash = ?, synced_at = ?,
        status = ?, last_error = ? WHERE id = ?`,
		hash, ts(now()), status, errText, id)
	return err
}

func (s *Store) DeleteCertDelivery(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM cert_deliveries WHERE id = ?`, id)
	return err
}

// ReplaceCAInDeliveryBundles swaps oldID for newID in every delivery's bundle selection —
// the activation step that keeps an explicitly-selected bundle following its lineage when
// a rotation promotes the successor. In Go, not SQL: splitting a delimited list is exactly
// the transform the two dialects spell differently.
func (s *Store) ReplaceCAInDeliveryBundles(ctx context.Context, oldID, newID string) error {
	deliveries, err := s.AllCertDeliveries(ctx)
	if err != nil {
		return err
	}
	for _, d := range deliveries {
		ids := strings.Fields(d.BundleCAs)
		changed := false
		for i, id := range ids {
			if id == oldID {
				ids[i] = newID
				changed = true
			}
		}
		if !changed {
			continue
		}
		if _, err := s.exec(ctx, `UPDATE cert_deliveries SET bundle_cas = ? WHERE id = ?`,
			strings.Join(ids, " "), d.ID); err != nil {
			return err
		}
	}
	return nil
}
