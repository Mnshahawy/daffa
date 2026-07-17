package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EncryptionKey is the public half of an age keypair. There is deliberately no column for
// the private half: it is generated in memory, downloaded once by whoever asked, and never
// stored — the box can encrypt backups it cannot read. What Daffa manages is the part key
// management actually needs on the server: which recipients exist, what they are called,
// and who is accountable for the private half being kept somewhere safe.
type EncryptionKey struct {
	ID        string
	Name      string
	Recipient string // age1…, plaintext on purpose
	Source    string // generated | imported
	CreatedAt time.Time
	CreatedBy string
}

const keyCols = `id, name, recipient, source, created_at, created_by`

func scanKey(sc interface{ Scan(...any) error }) (*EncryptionKey, error) {
	var k EncryptionKey
	var createdBy sql.NullString
	var createdAt string
	err := sc.Scan(&k.ID, &k.Name, &k.Recipient, &k.Source, &createdAt, &createdBy)
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

func (s *Store) CreateEncryptionKey(ctx context.Context, k *EncryptionKey) error {
	if k.ID == "" {
		k.ID = "key_" + NewID()
	}
	if k.Source == "" {
		k.Source = "imported"
	}
	k.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO encryption_keys (`+keyCols+`)
        VALUES (?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.Recipient, k.Source, ts(k.CreatedAt), nullStr(k.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating encryption key: %w", err)
	}
	return nil
}

func (s *Store) EncryptionKeyByID(ctx context.Context, id string) (*EncryptionKey, error) {
	return scanKey(s.queryRow(ctx, `SELECT `+keyCols+` FROM encryption_keys WHERE id = ?`, id))
}

func (s *Store) ListEncryptionKeys(ctx context.Context) ([]*EncryptionKey, error) {
	rows, err := s.query(ctx, `SELECT `+keyCols+` FROM encryption_keys ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing encryption keys: %w", err)
	}
	defer rows.Close()
	var out []*EncryptionKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// EncryptionKeyInUse counts backup jobs still encrypting to this key. Deleting a key a
// job depends on is refused: it would silently narrow who can restore those backups.
func (s *Store) EncryptionKeyInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM backup_job_keys WHERE key_id = ?`, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteEncryptionKey(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM encryption_keys WHERE id = ?`, id)
	return err
}
