package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Keyring is a stable name over an append-only set of key versions, so rotation can mean
// "new data uses the new key" instead of "all old data is now unreadable". The material
// lives in keyring_versions; this row is the identity and the rotation policy.
//
// Unlike every other sealed value in Daffa, a keyring version has no off-box source of
// truth: the material is generated here and never shown to anyone, so the sealed row is
// the only durable copy in existence. See docs/keyrings.md §5.
type Keyring struct {
	ID         string
	Name       string // also the filename prefix deliveries write
	RotateDays int    // 0 = manual rotation only
	CreatedAt  time.Time
	CreatedBy  string
}

// KeyringVersion is one 256-bit key. Its ID is the kid applications store beside their
// ciphertext to find the right version again at decrypt time.
type KeyringVersion struct {
	ID          string
	KeyringID   string
	MaterialEnc string // sealed 32 bytes; write-only forever
	State       string // active | decrypt_only | retired
	CreatedAt   time.Time
}

// Version states, and the only transitions: rotation demotes active to decrypt_only, the
// operator retires decrypt_only versions. Rows are never deleted — a retired version's
// row is the audit trail of what existed.
const (
	KeyringVersionActive      = "active"
	KeyringVersionDecryptOnly = "decrypt_only"
	KeyringVersionRetired     = "retired"
)

const keyringCols = `id, name, rotate_days, created_at, created_by`

func scanKeyring(sc interface{ Scan(...any) error }) (*Keyring, error) {
	var k Keyring
	var createdBy sql.NullString
	var createdAt string
	err := sc.Scan(&k.ID, &k.Name, &k.RotateDays, &createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.CreatedAt = parseTS(createdAt)
	k.CreatedBy = createdBy.String
	return &k, nil
}

func (s *Store) CreateKeyring(ctx context.Context, k *Keyring) error {
	if k.ID == "" {
		k.ID = "kr_" + NewID()
	}
	k.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO keyrings (`+keyringCols+`) VALUES (?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.RotateDays, ts(k.CreatedAt), nullStr(k.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating keyring: %w", err)
	}
	return nil
}

func (s *Store) KeyringByID(ctx context.Context, id string) (*Keyring, error) {
	return scanKeyring(s.queryRow(ctx, `SELECT `+keyringCols+` FROM keyrings WHERE id = ?`, id))
}

func (s *Store) ListKeyrings(ctx context.Context) ([]*Keyring, error) {
	rows, err := s.query(ctx, `SELECT `+keyringCols+` FROM keyrings ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing keyrings: %w", err)
	}
	defer rows.Close()
	var out []*Keyring
	for rows.Next() {
		k, err := scanKeyring(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) UpdateKeyring(ctx context.Context, k *Keyring) error {
	_, err := s.exec(ctx, `UPDATE keyrings SET name = ?, rotate_days = ? WHERE id = ?`,
		k.Name, k.RotateDays, k.ID)
	if err != nil {
		return fmt.Errorf("store: updating keyring: %w", err)
	}
	return nil
}

// KeyringInUse counts deliveries still carrying this keyring. Deleting a keyring a
// delivery depends on is refused: retiring versions is the graduated alternative.
func (s *Store) KeyringInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM keyring_deliveries WHERE keyring_id = ?`, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteKeyring(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM keyrings WHERE id = ?`, id)
	return err
}

// ── versions ────────────────────────────────────────────────────────────────────

const keyringVersionCols = `id, keyring_id, material_enc, state, created_at`

func scanKeyringVersion(sc interface{ Scan(...any) error }) (*KeyringVersion, error) {
	var v KeyringVersion
	var createdAt string
	err := sc.Scan(&v.ID, &v.KeyringID, &v.MaterialEnc, &v.State, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.CreatedAt = parseTS(createdAt)
	return &v, nil
}

// RotateKeyring appends a new active version and demotes the current one, atomically —
// two active versions would make "encrypt with current" ambiguous, and zero would strand
// the keyring. Works for the first version too, when there is nothing to demote.
func (s *Store) RotateKeyring(ctx context.Context, keyringID, materialEnc string) (*KeyringVersion, error) {
	defer s.lockWrites()()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: rotating keyring: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(
		`UPDATE keyring_versions SET state = ? WHERE keyring_id = ? AND state = ?`),
		KeyringVersionDecryptOnly, keyringID, KeyringVersionActive); err != nil {
		return nil, fmt.Errorf("store: demoting the active version: %w", err)
	}

	v := &KeyringVersion{
		ID: "krv_" + NewID(), KeyringID: keyringID,
		MaterialEnc: materialEnc, State: KeyringVersionActive, CreatedAt: now(),
	}
	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO keyring_versions (`+keyringVersionCols+`) VALUES (?, ?, ?, ?, ?)`),
		v.ID, v.KeyringID, v.MaterialEnc, v.State, ts(v.CreatedAt)); err != nil {
		return nil, fmt.Errorf("store: inserting the new version: %w", err)
	}
	return v, tx.Commit()
}

func (s *Store) KeyringVersionByID(ctx context.Context, id string) (*KeyringVersion, error) {
	return scanKeyringVersion(s.queryRow(ctx,
		`SELECT `+keyringVersionCols+` FROM keyring_versions WHERE id = ?`, id))
}

// KeyringVersions returns every version of a keyring, newest first — retired ones
// included, because the page that shows the timeline is also the audit trail.
func (s *Store) KeyringVersions(ctx context.Context, keyringID string) ([]*KeyringVersion, error) {
	rows, err := s.query(ctx, `SELECT `+keyringVersionCols+` FROM keyring_versions
        WHERE keyring_id = ? ORDER BY created_at DESC, id DESC`, keyringID)
	if err != nil {
		return nil, fmt.Errorf("store: listing keyring versions: %w", err)
	}
	defer rows.Close()
	var out []*KeyringVersion
	for rows.Next() {
		v, err := scanKeyringVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RetireKeyringVersion moves a decrypt_only version to retired. The state guard is in the
// WHERE on purpose: retiring the ACTIVE version would strand the keyring with nothing to
// encrypt with, and a race between "retire" and "rotate" must not slip past a check the
// handler did on stale data. Zero rows affected means the caller was refused.
func (s *Store) RetireKeyringVersion(ctx context.Context, id string) (bool, error) {
	res, err := s.exec(ctx, `UPDATE keyring_versions SET state = ? WHERE id = ? AND state = ?`,
		KeyringVersionRetired, id, KeyringVersionDecryptOnly)
	if err != nil {
		return false, fmt.Errorf("store: retiring keyring version: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ── deliveries ──────────────────────────────────────────────────────────────────

// KeyringDelivery keeps one keyring's live versions current inside a named volume on one
// host. The reconciler compares SyncedHash against the desired content and rewrites the
// volume when they differ — the cert_deliveries machinery with a different payload.
type KeyringDelivery struct {
	ID             string
	KeyringID      string
	EnvID          string
	Volume         string
	UID            int
	GID            int
	RestartTargets string // space-separated container names; empty = consumer re-reads

	SyncedHash string
	SyncedAt   time.Time
	Status     string // pending | ok | error
	LastError  string
	CreatedAt  time.Time
	CreatedBy  string
}

const keyringDeliveryCols = `id, keyring_id, env_id, volume, uid, gid, restart_targets,
    synced_hash, synced_at, status, last_error, created_at, created_by`

func scanKeyringDelivery(sc interface{ Scan(...any) error }) (*KeyringDelivery, error) {
	var d KeyringDelivery
	var syncedAt, createdBy sql.NullString
	var createdAt string
	err := sc.Scan(&d.ID, &d.KeyringID, &d.EnvID, &d.Volume, &d.UID, &d.GID,
		&d.RestartTargets, &d.SyncedHash, &syncedAt, &d.Status, &d.LastError,
		&createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if syncedAt.Valid {
		d.SyncedAt = parseTS(syncedAt.String)
	}
	d.CreatedAt = parseTS(createdAt)
	d.CreatedBy = createdBy.String
	return &d, nil
}

func (s *Store) CreateKeyringDelivery(ctx context.Context, d *KeyringDelivery) error {
	if d.ID == "" {
		d.ID = "kdl_" + NewID()
	}
	if d.Volume == "" {
		d.Volume = "daffa-keys"
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	d.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO keyring_deliveries (`+keyringDeliveryCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.KeyringID, d.EnvID, d.Volume, d.UID, d.GID, d.RestartTargets,
		d.SyncedHash, nullTS(d.SyncedAt), d.Status, d.LastError,
		ts(d.CreatedAt), nullStr(d.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating keyring delivery: %w", err)
	}
	return nil
}

func (s *Store) KeyringDeliveryByID(ctx context.Context, id string) (*KeyringDelivery, error) {
	return scanKeyringDelivery(s.queryRow(ctx,
		`SELECT `+keyringDeliveryCols+` FROM keyring_deliveries WHERE id = ?`, id))
}

// ListKeyringDeliveries returns the deliveries on hosts the caller may see.
func (s *Store) ListKeyringDeliveries(ctx context.Context, global bool, envs []string) ([]*KeyringDelivery, error) {
	where, args := envIn(global, envs)
	if where == neverMatches {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT `+keyringDeliveryCols+` FROM keyring_deliveries`+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing keyring deliveries: %w", err)
	}
	defer rows.Close()
	var out []*KeyringDelivery
	for rows.Next() {
		d, err := scanKeyringDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AllKeyringDeliveries returns every delivery, ignoring permissions. It is for the
// reconciler, which runs on behalf of the system and not of any user.
func (s *Store) AllKeyringDeliveries(ctx context.Context) ([]*KeyringDelivery, error) {
	return s.ListKeyringDeliveries(ctx, true, nil)
}

// MarkKeyringDeliverySynced records a reconcile outcome — the hash on success, the error
// verbatim on failure.
func (s *Store) MarkKeyringDeliverySynced(ctx context.Context, id, hash string, syncErr error) error {
	status, errText := "ok", ""
	if syncErr != nil {
		status, errText = "error", syncErr.Error()
	}
	_, err := s.exec(ctx, `UPDATE keyring_deliveries SET synced_hash = ?, synced_at = ?,
        status = ?, last_error = ? WHERE id = ?`,
		hash, ts(now()), status, errText, id)
	return err
}

func (s *Store) DeleteKeyringDelivery(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM keyring_deliveries WHERE id = ?`, id)
	return err
}
