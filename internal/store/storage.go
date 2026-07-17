package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StorageTarget is a bucket you can point several backup jobs at. Retyping an endpoint
// and a secret key for every database is how one of them ends up subtly wrong, and you
// find out when you need the backup.
type StorageTarget struct {
	ID        string
	Name      string
	Endpoint  string
	Region    string
	Bucket    string
	KeyID     string
	SecretEnc string
	CreatedAt time.Time
	CreatedBy string
}

const storageCols = `id, name, endpoint, region, bucket, key_id, secret_enc, created_at, created_by`

func scanStorage(sc interface{ Scan(...any) error }) (*StorageTarget, error) {
	var t StorageTarget
	var createdBy sql.NullString
	var createdAt string
	err := sc.Scan(&t.ID, &t.Name, &t.Endpoint, &t.Region, &t.Bucket, &t.KeyID, &t.SecretEnc,
		&createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedBy = createdBy.String
	t.CreatedAt = parseTS(createdAt)
	return &t, nil
}

func (s *Store) CreateStorageTarget(ctx context.Context, t *StorageTarget) error {
	if t.ID == "" {
		t.ID = NewID()
	}
	t.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO storage_targets (`+storageCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Endpoint, t.Region, t.Bucket, t.KeyID, t.SecretEnc,
		ts(t.CreatedAt), nullStr(t.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating storage target: %w", err)
	}
	return nil
}

func (s *Store) StorageTargetByID(ctx context.Context, id string) (*StorageTarget, error) {
	return scanStorage(s.queryRow(ctx, `SELECT `+storageCols+` FROM storage_targets WHERE id = ?`, id))
}

func (s *Store) ListStorageTargets(ctx context.Context) ([]*StorageTarget, error) {
	rows, err := s.query(ctx, `SELECT `+storageCols+` FROM storage_targets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing storage targets: %w", err)
	}
	defer rows.Close()

	var out []*StorageTarget
	for rows.Next() {
		t, err := scanStorage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateStorageTarget(ctx context.Context, t *StorageTarget) error {
	_, err := s.exec(ctx, `UPDATE storage_targets SET name = ?, endpoint = ?, region = ?,
        bucket = ?, key_id = ?, secret_enc = ? WHERE id = ?`,
		t.Name, t.Endpoint, t.Region, t.Bucket, t.KeyID, t.SecretEnc, t.ID)
	return err
}

// StorageTargetInUse reports how many backup jobs depend on this target, so deleting one
// out from under a job is something we can refuse rather than something you discover
// when the next backup fails.
func (s *Store) StorageTargetInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM backup_jobs WHERE storage_id = ?`, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteStorageTarget(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM storage_targets WHERE id = ?`, id)
	return err
}
