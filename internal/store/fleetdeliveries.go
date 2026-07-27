package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FleetGroup is one subdirectory of a fleet delivery's volume (” = the volume root):
// which certificates land there and which roots its ca-bundle.crt carries.
//
// BundleCAs ” means DERIVED — the lineages of the CAs that issued this group's
// certificates — not "all managed CAs" as it does on cert_deliveries. A fleet volume
// aggregates many trust domains on purpose, and defaulting any of them to the full
// bundle would hand a consumer every root exactly where trust is being kept apart.
type FleetGroup struct {
	Subdir    string
	BundleCAs string   // space-separated CA ids; '' = derived from the group's issuers
	CertIDs   []string // ordered by certificate name, so rendered content is deterministic
}

// FleetDelivery composes certificates from ANY environment — grouped into
// subdirectories, each with its own trust bundle — into one named volume on the
// CONSUMER's environment. It is deliberately a separate entity from CertDelivery:
// that one's contract is "material stays inside its own environment", and the fleet
// case is the sanctioned exception, behind its own capability (fleet.edit), its own
// routes, and its own volume manifest. See fleet-deliveries.md.
type FleetDelivery struct {
	ID             string
	EnvID          string // the consumer's environment — where the volume lives
	Volume         string
	UID            int
	GID            int
	RestartTargets string // space-separated container names; empty = consumer hot-reloads
	Groups         []FleetGroup

	SyncedHash string
	SyncedAt   time.Time
	Status     string // pending | ok | error
	LastError  string
	CreatedAt  time.Time
	CreatedBy  string
}

const fleetDeliveryCols = `id, env_id, volume, uid, gid, restart_targets,
    synced_hash, synced_at, status, last_error, created_at, created_by`

func scanFleetDelivery(sc interface{ Scan(...any) error }) (*FleetDelivery, error) {
	var d FleetDelivery
	var syncedAt, createdBy sql.NullString
	var createdAt string
	err := sc.Scan(&d.ID, &d.EnvID, &d.Volume, &d.UID, &d.GID, &d.RestartTargets,
		&d.SyncedHash, &syncedAt, &d.Status, &d.LastError, &createdAt, &createdBy)
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

func (s *Store) CreateFleetDelivery(ctx context.Context, d *FleetDelivery) error {
	if d.ID == "" {
		d.ID = "fdl_" + NewID()
	}
	if d.Volume == "" {
		d.Volume = "daffa-fleet-certs"
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	d.CreatedAt = now()

	// The row and its groups go in together, or neither does — the CertDelivery
	// reasoning: a committed delivery whose groups failed to attach would reconcile to
	// an empty volume and quietly prune the material a consumer is running on.
	defer s.lockWrites()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: creating fleet delivery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO fleet_deliveries (`+fleetDeliveryCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		d.ID, d.EnvID, d.Volume, d.UID, d.GID, d.RestartTargets,
		d.SyncedHash, nullTS(d.SyncedAt), d.Status, d.LastError,
		ts(d.CreatedAt), nullStr(d.CreatedBy)); err != nil {
		return fmt.Errorf("store: creating fleet delivery: %w", err)
	}
	if err := setFleetGroupsTx(ctx, s, tx, d.ID, d.Groups); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateFleetDelivery replaces the editable state — the groups and the write options.
// env_id and volume are not editable, for the CertDelivery reason: they are what the
// consumer's mount is keyed on, so moving either is a new delivery, not an edit.
func (s *Store) UpdateFleetDelivery(ctx context.Context, d *FleetDelivery) error {
	defer s.lockWrites()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: updating fleet delivery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE fleet_deliveries SET uid = ?, gid = ?,
        restart_targets = ? WHERE id = ?`),
		d.UID, d.GID, d.RestartTargets, d.ID); err != nil {
		return fmt.Errorf("store: updating fleet delivery: %w", err)
	}
	if err := setFleetGroupsTx(ctx, s, tx, d.ID, d.Groups); err != nil {
		return err
	}
	return tx.Commit()
}

func setFleetGroupsTx(ctx context.Context, s *Store, tx *sql.Tx, deliveryID string, groups []FleetGroup) error {
	if _, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM fleet_delivery_groups WHERE delivery_id = ?`), deliveryID); err != nil {
		return fmt.Errorf("store: clearing fleet delivery groups: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM fleet_delivery_certs WHERE delivery_id = ?`), deliveryID); err != nil {
		return fmt.Errorf("store: clearing fleet delivery certificates: %w", err)
	}
	for _, g := range groups {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO fleet_delivery_groups (delivery_id, subdir, bundle_cas) VALUES (?, ?, ?)`),
			deliveryID, g.Subdir, g.BundleCAs); err != nil {
			return fmt.Errorf("store: adding group %q to the fleet delivery: %w", g.Subdir, err)
		}
		for _, certID := range g.CertIDs {
			if _, err := tx.ExecContext(ctx, s.rebind(
				`INSERT INTO fleet_delivery_certs (delivery_id, cert_id, subdir) VALUES (?, ?, ?)`),
				deliveryID, certID, g.Subdir); err != nil {
				return fmt.Errorf("store: adding certificate %s to the fleet delivery: %w", certID, err)
			}
		}
	}
	return nil
}

// attachFleetGroups loads the groups and their certificates for a batch of deliveries in
// two queries, not two per delivery. Groups come back ordered by subdir and certificates
// by NAME, because that order becomes the rendered file order — and therefore part of the
// content hash, which must not depend on how rows happen to come back.
func (s *Store) attachFleetGroups(ctx context.Context, deliveries []*FleetDelivery) error {
	if len(deliveries) == 0 {
		return nil
	}
	byID := make(map[string]*FleetDelivery, len(deliveries))
	for _, d := range deliveries {
		byID[d.ID] = d
	}

	groupIdx := map[string]map[string]int{} // delivery id → subdir → index in Groups
	rows, err := s.query(ctx, `SELECT delivery_id, subdir, bundle_cas
        FROM fleet_delivery_groups ORDER BY subdir`)
	if err != nil {
		return fmt.Errorf("store: loading fleet delivery groups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var deliveryID string
		var g FleetGroup
		if err := rows.Scan(&deliveryID, &g.Subdir, &g.BundleCAs); err != nil {
			return err
		}
		d, ok := byID[deliveryID]
		if !ok {
			continue
		}
		if groupIdx[deliveryID] == nil {
			groupIdx[deliveryID] = map[string]int{}
		}
		groupIdx[deliveryID][g.Subdir] = len(d.Groups)
		d.Groups = append(d.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	crows, err := s.query(ctx, `SELECT fc.delivery_id, fc.cert_id, fc.subdir
        FROM fleet_delivery_certs fc
        JOIN certificates c ON c.id = fc.cert_id ORDER BY c.name`)
	if err != nil {
		return fmt.Errorf("store: loading fleet delivery certificates: %w", err)
	}
	defer crows.Close()
	for crows.Next() {
		var deliveryID, certID, subdir string
		if err := crows.Scan(&deliveryID, &certID, &subdir); err != nil {
			return err
		}
		d, ok := byID[deliveryID]
		if !ok {
			continue
		}
		if i, ok := groupIdx[deliveryID][subdir]; ok {
			d.Groups[i].CertIDs = append(d.Groups[i].CertIDs, certID)
		}
	}
	return crows.Err()
}

func (s *Store) FleetDeliveryByID(ctx context.Context, id string) (*FleetDelivery, error) {
	d, err := scanFleetDelivery(s.queryRow(ctx,
		`SELECT `+fleetDeliveryCols+` FROM fleet_deliveries WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.attachFleetGroups(ctx, []*FleetDelivery{d}); err != nil {
		return nil, err
	}
	return d, nil
}

// FleetDeliveryForVolume finds the fleet delivery writing a volume on an environment, if
// any. It is one half of the volume-exclusivity rule: a fleet delivery and a certificate
// delivery each mirror their own manifest, so two of them pruning one volume would
// eventually delete each other's files. ErrNotFound means the volume is free.
func (s *Store) FleetDeliveryForVolume(ctx context.Context, envID, volume string) (*FleetDelivery, error) {
	d, err := scanFleetDelivery(s.queryRow(ctx, `SELECT `+fleetDeliveryCols+` FROM fleet_deliveries
        WHERE env_id = ? AND volume = ?`, envID, volume))
	if err != nil {
		return nil, err
	}
	if err := s.attachFleetGroups(ctx, []*FleetDelivery{d}); err != nil {
		return nil, err
	}
	return d, nil
}

// ListFleetDeliveries returns the fleet deliveries whose CONSUMER environment the caller
// may see. What a delivery carries may name other environments — that is the feature —
// and the view layer shows those names; visibility is keyed on where the volume lives.
func (s *Store) ListFleetDeliveries(ctx context.Context, global bool, envs []string) ([]*FleetDelivery, error) {
	where, args := envIn(global, envs)
	if where == neverMatches {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT `+fleetDeliveryCols+` FROM fleet_deliveries`+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing fleet deliveries: %w", err)
	}
	defer rows.Close()
	var out []*FleetDelivery
	for rows.Next() {
		d, err := scanFleetDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachFleetGroups(ctx, out)
}

// AllFleetDeliveries returns every fleet delivery, ignoring permissions. It is for the
// reconciler, which runs on behalf of the system and not of any user.
func (s *Store) AllFleetDeliveries(ctx context.Context) ([]*FleetDelivery, error) {
	return s.ListFleetDeliveries(ctx, true, nil)
}

// MarkFleetDeliverySynced records a reconcile outcome — the hash on success, the error
// verbatim on failure.
func (s *Store) MarkFleetDeliverySynced(ctx context.Context, id, hash string, syncErr error) error {
	status, errText := "ok", ""
	if syncErr != nil {
		status, errText = "error", syncErr.Error()
	}
	_, err := s.exec(ctx, `UPDATE fleet_deliveries SET synced_hash = ?, synced_at = ?,
        status = ?, last_error = ? WHERE id = ?`,
		hash, ts(now()), status, errText, id)
	return err
}

func (s *Store) DeleteFleetDelivery(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM fleet_deliveries WHERE id = ?`, id)
	return err
}

// ReplaceCAInFleetBundles swaps oldID for newID in every fleet group's explicit bundle
// selection — the same activation step as ReplaceCAInDeliveryBundles, for the same
// reason: an explicitly-selected bundle must follow its lineage when a rotation promotes
// the successor. Derived groups (”) need nothing: derivation follows the certificates'
// ca_id, which rotation re-points when it re-signs the leaves.
func (s *Store) ReplaceCAInFleetBundles(ctx context.Context, oldID, newID string) error {
	deliveries, err := s.AllFleetDeliveries(ctx)
	if err != nil {
		return err
	}
	for _, d := range deliveries {
		for _, g := range d.Groups {
			ids := strings.Fields(g.BundleCAs)
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
			if _, err := s.exec(ctx, `UPDATE fleet_delivery_groups SET bundle_cas = ?
                WHERE delivery_id = ? AND subdir = ?`,
				strings.Join(ids, " "), d.ID, g.Subdir); err != nil {
				return err
			}
		}
	}
	return nil
}
