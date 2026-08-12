package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SecretValue is one generated secret: a value Daffa minted at first manifest apply,
// sealed, and injects into every manifest slot that references it. Generated ONCE —
// nothing here ever rewrites value_enc; rotation, when it exists, will be its own
// deliberate verb. format/length are the declared generation parameters, kept so a
// manifest that later declares different ones reads as drift.
type SecretValue struct {
	ID        string
	Name      string
	ValueEnc  string
	Format    string
	Length    int
	CreatedAt time.Time
	CreatedBy string
}

const secretValueCols = `id, name, value_enc, format, length, created_at, created_by`

func scanSecretValue(sc interface{ Scan(...any) error }) (*SecretValue, error) {
	var v SecretValue
	var createdAt string
	err := sc.Scan(&v.ID, &v.Name, &v.ValueEnc, &v.Format, &v.Length, &createdAt, &v.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.CreatedAt = parseTS(createdAt)
	return &v, nil
}

func (s *Store) CreateSecretValue(ctx context.Context, v *SecretValue) error {
	if v.ID == "" {
		v.ID = "sec_" + NewID()
	}
	v.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO secret_values (`+secretValueCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.Name, v.ValueEnc, v.Format, v.Length, ts(v.CreatedAt), v.CreatedBy)
	if err != nil {
		return fmt.Errorf("store: creating secret value %q: %w", v.Name, err)
	}
	return nil
}

func (s *Store) SecretValueByName(ctx context.Context, name string) (*SecretValue, error) {
	return scanSecretValue(s.queryRow(ctx,
		`SELECT `+secretValueCols+` FROM secret_values WHERE name = ?`, name))
}
