package store

// Webhooks: the endpoint a forge pushes to, as an object of its own that any number of
// stacks and volume sources link to. What a push DOES to each target — which repo, branch
// and paths it must match, whether the target is paused — stays on the target; the webhook
// is only the authenticated door. See .ai/webhooks.md.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Webhook struct {
	ID    string
	Name  string
	EnvID string // "" = fleet-wide
	// SecretEnc is the sealed shared secret the forge signs with. Write-only through the
	// API: shown once when minted or rotated.
	SecretEnc string

	LastDeliveryAt time.Time
	// LastDelivery is the JSON the delivery handler recorded — opaque to the store.
	LastDelivery string

	CreatedAt time.Time
	CreatedBy string
}

const webhookCols = `id, name, env_id, secret_enc, last_delivery_at, last_delivery, created_at, created_by`

func scanWebhook(sc interface{ Scan(...any) error }) (*Webhook, error) {
	var w Webhook
	var envID, lastAt, createdBy sql.NullString
	var createdAt string
	err := sc.Scan(&w.ID, &w.Name, &envID, &w.SecretEnc, &lastAt, &w.LastDelivery, &createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.EnvID = envID.String
	w.CreatedBy = createdBy.String
	w.CreatedAt = parseTS(createdAt)
	if lastAt.Valid {
		w.LastDeliveryAt = parseTS(lastAt.String)
	}
	return &w, nil
}

// ErrWebhookNameTaken is the unique name index speaking; the API owes a 400 naming it.
var ErrWebhookNameTaken = errors.New("store: a webhook with that name already exists")

// isUniqueViolation recognises the name index in both dialects' words — SQLite's "UNIQUE
// constraint failed" and Postgres's "violates unique constraint" — the same test the
// volume-source handler makes, kept here so the API sees a sentinel instead.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

func (s *Store) CreateWebhook(ctx context.Context, w *Webhook) error {
	if w.ID == "" {
		w.ID = "whk_" + NewID()
	}
	w.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO webhooks (id, name, env_id, secret_enc, last_delivery, created_at, created_by)
        VALUES (?, ?, ?, ?, '', ?, ?)`,
		w.ID, w.Name, nullStr(w.EnvID), w.SecretEnc, ts(w.CreatedAt), nullStr(w.CreatedBy))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrWebhookNameTaken
		}
		return fmt.Errorf("store: creating webhook: %w", err)
	}
	return nil
}

func (s *Store) WebhookByID(ctx context.Context, id string) (*Webhook, error) {
	return scanWebhook(s.queryRow(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
}

// WebhookByName resolves the name a manifest declares; webhooks.name is UNIQUE.
func (s *Store) WebhookByName(ctx context.Context, name string) (*Webhook, error) {
	return scanWebhook(s.queryRow(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE name = ?`, name))
}

// ListWebhooks returns the fleet-wide webhooks plus those on the clusters the caller may
// see. Fleet-wide ones are visible to every holder, at any scope, for the credential-store
// reason: a cluster-scoped operator has to be able to pick one to link.
func (s *Store) ListWebhooks(ctx context.Context, global bool, envs []string) ([]*Webhook, error) {
	where, args := envIn(global, envs)
	switch {
	case global:
	case where == neverMatches:
		where = ` WHERE env_id IS NULL`
	default:
		where += ` OR env_id IS NULL`
	}
	rows, err := s.query(ctx, `SELECT `+webhookCols+` FROM webhooks`+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing webhooks: %w", err)
	}
	defer rows.Close()
	var out []*Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) RenameWebhook(ctx context.Context, id, name string) error {
	_, err := s.exec(ctx, `UPDATE webhooks SET name = ? WHERE id = ?`, name, id)
	if isUniqueViolation(err) {
		return ErrWebhookNameTaken
	}
	return err
}

func (s *Store) SetWebhookSecret(ctx context.Context, id, sealed string) error {
	_, err := s.exec(ctx, `UPDATE webhooks SET secret_enc = ? WHERE id = ?`, sealed, id)
	return err
}

// RecordWebhookDelivery overwrites the last delivery. One row, not a history: the question
// is "what did the last push do?", and the per-target audit entries are the history.
func (s *Store) RecordWebhookDelivery(ctx context.Context, id, summaryJSON string) error {
	_, err := s.exec(ctx, `UPDATE webhooks SET last_delivery_at = ?, last_delivery = ? WHERE id = ?`,
		ts(now()), summaryJSON, id)
	return err
}

// WebhookInUse counts the stacks and volume sources linked to a webhook, so deleting one
// that still drives something is refused rather than leaving auto-deploys silently dead.
func (s *Store) WebhookInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT
        (SELECT COUNT(*) FROM stacks WHERE webhook_id = ?) +
        (SELECT COUNT(*) FROM volume_sources WHERE webhook_id = ?)`, id, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteWebhook(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	return err
}

// StacksByWebhook and VolumeSourcesByWebhook are a delivery's targets.
func (s *Store) StacksByWebhook(ctx context.Context, webhookID string) ([]*Stack, error) {
	rows, err := s.query(ctx, `SELECT `+stackReadCols+` FROM stacks WHERE webhook_id = ? ORDER BY name`, webhookID)
	if err != nil {
		return nil, fmt.Errorf("store: listing a webhook's stacks: %w", err)
	}
	defer rows.Close()
	var out []*Stack
	for rows.Next() {
		st, err := scanStack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) VolumeSourcesByWebhook(ctx context.Context, webhookID string) ([]*VolumeSource, error) {
	rows, err := s.query(ctx, `SELECT `+volSourceCols+` FROM volume_sources WHERE webhook_id = ? ORDER BY volume`, webhookID)
	if err != nil {
		return nil, fmt.Errorf("store: listing a webhook's volume sources: %w", err)
	}
	defer rows.Close()
	var out []*VolumeSource
	for rows.Next() {
		v, err := scanVolumeSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
