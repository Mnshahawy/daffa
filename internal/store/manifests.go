package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ManifestApply is one recorded plan or apply of a declarative manifest — the document
// as submitted, and the per-resource report it produced. History, not state: the live
// resources are their own rows; these rows answer "was this exact file ever applied,
// and what did it change?". The document is stored verbatim because the format cannot
// carry a secret by construction (internal/manifest.SecretRef) — see 0018's comment.
type ManifestApply struct {
	ID        string
	Name      string // the document's name: label
	DocHash   string
	Document  string
	Report    string // JSON verdict report
	AppliedBy string
	AppliedAt time.Time
	DryRun    bool // a plan: recorded, nothing executed
}

const manifestApplyCols = `id, name, doc_hash, document, report, applied_by, applied_at, dry_run`

func scanManifestApply(sc interface{ Scan(...any) error }) (*ManifestApply, error) {
	var m ManifestApply
	var appliedAt string
	var dryRun int
	err := sc.Scan(&m.ID, &m.Name, &m.DocHash, &m.Document, &m.Report,
		&m.AppliedBy, &appliedAt, &dryRun)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.AppliedAt = parseTS(appliedAt)
	m.DryRun = dryRun != 0
	return &m, nil
}

func (s *Store) CreateManifestApply(ctx context.Context, m *ManifestApply) error {
	if m.ID == "" {
		m.ID = "man_" + NewID()
	}
	m.AppliedAt = now()
	_, err := s.exec(ctx, `INSERT INTO manifest_applies (`+manifestApplyCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Name, m.DocHash, m.Document, m.Report, m.AppliedBy,
		ts(m.AppliedAt), boolInt(m.DryRun))
	if err != nil {
		return fmt.Errorf("store: recording manifest apply: %w", err)
	}
	return nil
}

func (s *Store) ManifestApplyByID(ctx context.Context, id string) (*ManifestApply, error) {
	return scanManifestApply(s.queryRow(ctx,
		`SELECT `+manifestApplyCols+` FROM manifest_applies WHERE id = ?`, id))
}

// ListManifestApplies returns the history newest-first. Global on purpose: a manifest
// spans hosts, so the viewing capability is global and there is nothing to filter by.
func (s *Store) ListManifestApplies(ctx context.Context) ([]*ManifestApply, error) {
	rows, err := s.query(ctx,
		`SELECT `+manifestApplyCols+` FROM manifest_applies ORDER BY applied_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: listing manifest applies: %w", err)
	}
	defer rows.Close()
	var out []*ManifestApply
	for rows.Next() {
		m, err := scanManifestApply(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
