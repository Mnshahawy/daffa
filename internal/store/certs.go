package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
}

// CanSign reports whether Daffa holds this CA's private key.
func (ca *CertAuthority) CanSign() bool { return ca.KeyEnc != "" }

const caCols = `id, name, subject, cert_pem, key_enc, key_algo, not_before, not_after,
    status, rotates_id, overlap_until, warn_days, created_at, created_by, protected`

func scanCA(sc interface{ Scan(...any) error }) (*CertAuthority, error) {
	var ca CertAuthority
	var notBefore, notAfter, createdAt string
	var rotatesID, overlapUntil, createdBy sql.NullString
	var protected int
	err := sc.Scan(&ca.ID, &ca.Name, &ca.Subject, &ca.CertPEM, &ca.KeyEnc, &ca.KeyAlgo,
		&notBefore, &notAfter, &ca.Status, &rotatesID, &overlapUntil, &ca.WarnDays, &createdAt, &createdBy, &protected)
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
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ca.ID, ca.Name, ca.Subject, ca.CertPEM, ca.KeyEnc, ca.KeyAlgo,
		ts(ca.NotBefore), ts(ca.NotAfter), ca.Status, nullStr(ca.RotatesID),
		nullTS(ca.OverlapUntil), ca.WarnDays, ts(ca.CreatedAt), nullStr(ca.CreatedBy), boolInt(ca.Protected))
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
        overlap_until = ?, warn_days = ? WHERE id = ?`,
		ca.Subject, ca.CertPEM, ca.KeyEnc, ca.KeyAlgo, ts(ca.NotBefore), ts(ca.NotAfter),
		ca.Status, nullStr(ca.RotatesID), nullTS(ca.OverlapUntil), ca.WarnDays, ca.ID)
	return err
}

// CertAuthorityInUse counts live certificates issued by this CA, so deletion can be
// refused instead of orphaning them.
func (s *Store) CertAuthorityInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM certificates WHERE ca_id = ?`, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteCertAuthority(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM cert_authorities WHERE id = ?`, id)
	return err
}

// ── certificates ────────────────────────────────────────────────────────────────

// Certificate is a leaf: issued by one of Daffa's CAs (CAID set — renewable) or uploaded
// (CAID empty — tracked and delivered, but only its owner can renew it, by uploading again).
type Certificate struct {
	ID       string
	Name     string
	CAID     string
	SANs     string // space-separated; first entry is the CN
	KeyAlgo  string
	CertPEM  string
	ChainPEM string
	KeyEnc   string

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

const certCols = `id, name, ca_id, sans, key_algo, cert_pem, chain_pem, key_enc,
    not_before, not_after, validity_days, renew_before_days, status, last_error,
    created_at, created_by, protected`

func scanCert(sc interface{ Scan(...any) error }) (*Certificate, error) {
	var c Certificate
	var caID, createdBy sql.NullString
	var notBefore, notAfter, createdAt string
	var protected int
	err := sc.Scan(&c.ID, &c.Name, &caID, &c.SANs, &c.KeyAlgo, &c.CertPEM, &c.ChainPEM,
		&c.KeyEnc, &notBefore, &notAfter, &c.ValidityDays, &c.RenewBeforeDays,
		&c.Status, &c.LastError, &createdAt, &createdBy, &protected)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
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
	c.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO certificates (`+certCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, nullStr(c.CAID), c.SANs, c.KeyAlgo, c.CertPEM, c.ChainPEM, c.KeyEnc,
		ts(c.NotBefore), ts(c.NotAfter), c.ValidityDays, c.RenewBeforeDays,
		c.Status, c.LastError, ts(c.CreatedAt), nullStr(c.CreatedBy), boolInt(c.Protected))
	if err != nil {
		return fmt.Errorf("store: creating certificate: %w", err)
	}
	return nil
}

func (s *Store) CertificateByID(ctx context.Context, id string) (*Certificate, error) {
	return scanCert(s.queryRow(ctx, `SELECT `+certCols+` FROM certificates WHERE id = ?`, id))
}

func (s *Store) ListCertificates(ctx context.Context) ([]*Certificate, error) {
	rows, err := s.query(ctx, `SELECT `+certCols+` FROM certificates ORDER BY name`)
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

func (s *Store) UpdateCertificate(ctx context.Context, c *Certificate) error {
	_, err := s.exec(ctx, `UPDATE certificates SET name = ?, ca_id = ?, sans = ?, key_algo = ?,
        cert_pem = ?, chain_pem = ?, key_enc = ?, not_before = ?, not_after = ?,
        validity_days = ?, renew_before_days = ?, status = ?, last_error = ? WHERE id = ?`,
		c.Name, nullStr(c.CAID), c.SANs, c.KeyAlgo, c.CertPEM, c.ChainPEM, c.KeyEnc,
		ts(c.NotBefore), ts(c.NotAfter), c.ValidityDays, c.RenewBeforeDays,
		c.Status, c.LastError, c.ID)
	return err
}

// CertificateInUse counts deliveries carrying this cert, so deletion can be refused while
// something still consumes it.
func (s *Store) CertificateInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM cert_deliveries WHERE cert_id = ?`, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteCertificate(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM certificates WHERE id = ?`, id)
	return err
}

// ── deliveries ──────────────────────────────────────────────────────────────────

// CertDelivery keeps one certificate (or just the trust bundle) current inside a named
// volume on one host. The reconciler compares SyncedHash against the desired content and
// rewrites the volume when they differ.
type CertDelivery struct {
	ID     string
	EnvID  string
	CertID string // empty = trust-bundle-only
	Volume string
	UID    int
	GID    int
	// Traefik also renders a file-provider tls.yml into the volume, so Traefik
	// hot-reloads instead of restarting.
	Traefik        bool
	RestartTargets string // space-separated container names; empty = consumer hot-reloads

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

const deliveryCols = `id, env_id, cert_id, volume, uid, gid, traefik, restart_targets,
    synced_hash, synced_at, status, last_error, created_at, created_by, protected`

func scanDelivery(sc interface{ Scan(...any) error }) (*CertDelivery, error) {
	var d CertDelivery
	var certID, syncedAt, createdBy sql.NullString
	var createdAt string
	var traefik, protected int
	err := sc.Scan(&d.ID, &d.EnvID, &certID, &d.Volume, &d.UID, &d.GID, &traefik,
		&d.RestartTargets, &d.SyncedHash, &syncedAt, &d.Status, &d.LastError,
		&createdAt, &createdBy, &protected)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.CertID = certID.String
	d.Traefik = traefik != 0
	if syncedAt.Valid {
		d.SyncedAt = parseTS(syncedAt.String)
	}
	d.CreatedAt = parseTS(createdAt)
	d.CreatedBy = createdBy.String
	d.Protected = protected != 0
	return &d, nil
}

func (s *Store) CreateCertDelivery(ctx context.Context, d *CertDelivery) error {
	if d.ID == "" {
		d.ID = "dlv_" + NewID()
	}
	if d.Volume == "" {
		d.Volume = "daffa-certs"
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	d.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO cert_deliveries (`+deliveryCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.EnvID, nullStr(d.CertID), d.Volume, d.UID, d.GID, boolInt(d.Traefik),
		d.RestartTargets, d.SyncedHash, nullTS(d.SyncedAt), d.Status, d.LastError,
		ts(d.CreatedAt), nullStr(d.CreatedBy), boolInt(d.Protected))
	if err != nil {
		return fmt.Errorf("store: creating certificate delivery: %w", err)
	}
	return nil
}

func (s *Store) CertDeliveryByID(ctx context.Context, id string) (*CertDelivery, error) {
	return scanDelivery(s.queryRow(ctx, `SELECT `+deliveryCols+` FROM cert_deliveries WHERE id = ?`, id))
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
	return out, rows.Err()
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
