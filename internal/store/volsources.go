package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// VolumeSource declares that a named volume's contents come from a git subtree — Daffa
// creates the volume, fills it, and keeps it current. Its existence is what makes a
// volume config-shaped: the repo is the source of truth and the volume holds disposable
// copies. See docs/volumes.md.
type VolumeSource struct {
	ID     string
	EnvID  string
	Volume string

	// SourceKind is git | inline. Git syncs a subtree of a repository; inline delivers a set
	// of files authored in Daffa (VolSourceFile) — what an inline stack with no repo needs.
	SourceKind string

	GitURL          string
	GitRef          string
	GitPath         string // the subtree; empty = repository root
	GitCredentialID string

	UID int
	GID int

	// StackID links this source to a stack whose deploys sync it first. Nullable; the
	// source outlives the stack.
	StackID string
	// RestartTargets are bounced after a sync that changed content — for consumers that
	// cannot hot-reload. Space-separated container names; empty = consumer hot-reloads.
	RestartTargets string

	AutoSync         bool
	WebhookSecretEnc string

	SyncedHash   string
	SyncedCommit string
	SyncedAt     time.Time
	Status       string // pending | ok | error
	LastError    string
	// Warnings are true statements about content that synced anyway, newline-separated —
	// say-so, then defer to the operator.
	Warnings string

	CreatedAt time.Time
	CreatedBy string
}

const volSourceCols = `id, env_id, volume, source_kind, git_url, git_ref, git_path, git_credential_id,
    uid, gid, stack_id, restart_targets, auto_sync, webhook_secret_enc,
    synced_hash, synced_commit, synced_at, status, last_error, warnings, created_at, created_by`

func scanVolumeSource(sc interface{ Scan(...any) error }) (*VolumeSource, error) {
	var v VolumeSource
	var credID, stackID, syncedAt, createdBy sql.NullString
	var createdAt string
	var autoSync int
	err := sc.Scan(&v.ID, &v.EnvID, &v.Volume, &v.SourceKind, &v.GitURL, &v.GitRef, &v.GitPath, &credID,
		&v.UID, &v.GID, &stackID, &v.RestartTargets, &autoSync, &v.WebhookSecretEnc,
		&v.SyncedHash, &v.SyncedCommit, &syncedAt, &v.Status, &v.LastError, &v.Warnings,
		&createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.GitCredentialID = credID.String
	v.StackID = stackID.String
	v.AutoSync = autoSync != 0
	if syncedAt.Valid {
		v.SyncedAt = parseTS(syncedAt.String)
	}
	v.CreatedAt = parseTS(createdAt)
	v.CreatedBy = createdBy.String
	return &v, nil
}

func (s *Store) CreateVolumeSource(ctx context.Context, v *VolumeSource) error {
	if v.ID == "" {
		v.ID = "vsr_" + NewID()
	}
	if v.Status == "" {
		v.Status = "pending"
	}
	if v.SourceKind == "" {
		v.SourceKind = "git"
	}
	v.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO volume_sources (`+volSourceCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.EnvID, v.Volume, v.SourceKind, v.GitURL, v.GitRef, v.GitPath, nullStr(v.GitCredentialID),
		v.UID, v.GID, nullStr(v.StackID), v.RestartTargets, boolInt(v.AutoSync),
		v.WebhookSecretEnc, v.SyncedHash, v.SyncedCommit, nullTS(v.SyncedAt), v.Status,
		v.LastError, v.Warnings, ts(v.CreatedAt), nullStr(v.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating volume source: %w", err)
	}
	return nil
}

func (s *Store) VolumeSourceByID(ctx context.Context, id string) (*VolumeSource, error) {
	return scanVolumeSource(s.queryRow(ctx, `SELECT `+volSourceCols+` FROM volume_sources WHERE id = ?`, id))
}

// VolumeSourceByVolume finds the source attached to a named volume on one host, if any.
// It is what the volume-delete guard and the volumes list ask.
func (s *Store) VolumeSourceByVolume(ctx context.Context, envID, volume string) (*VolumeSource, error) {
	return scanVolumeSource(s.queryRow(ctx,
		`SELECT `+volSourceCols+` FROM volume_sources WHERE env_id = ? AND volume = ?`, envID, volume))
}

// ListVolumeSources returns the sources on hosts the caller may see.
func (s *Store) ListVolumeSources(ctx context.Context, global bool, envs []string) ([]*VolumeSource, error) {
	where, args := envIn(global, envs)
	if where == neverMatches {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT `+volSourceCols+` FROM volume_sources`+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing volume sources: %w", err)
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

// VolumeSourcesByStack returns the sources a stack's deploys must sync first.
func (s *Store) VolumeSourcesByStack(ctx context.Context, stackID string) ([]*VolumeSource, error) {
	rows, err := s.query(ctx, `SELECT `+volSourceCols+` FROM volume_sources WHERE stack_id = ? ORDER BY volume`, stackID)
	if err != nil {
		return nil, fmt.Errorf("store: listing volume sources for stack: %w", err)
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

// UpdateVolumeSource rewrites the mutable fields. EnvID and Volume are not among them:
// retargeting a source would strand the old volume with a manifest nothing owns —
// delete and recreate instead, so both halves are explicit.
func (s *Store) UpdateVolumeSource(ctx context.Context, v *VolumeSource) error {
	_, err := s.exec(ctx, `UPDATE volume_sources SET git_url = ?, git_ref = ?, git_path = ?,
        git_credential_id = ?, uid = ?, gid = ?, stack_id = ?, restart_targets = ?,
        auto_sync = ?, webhook_secret_enc = ? WHERE id = ?`,
		v.GitURL, v.GitRef, v.GitPath, nullStr(v.GitCredentialID), v.UID, v.GID,
		nullStr(v.StackID), v.RestartTargets, boolInt(v.AutoSync), v.WebhookSecretEnc, v.ID)
	if err != nil {
		return fmt.Errorf("store: updating volume source: %w", err)
	}
	return nil
}

// MarkVolumeSourceSynced records a reconcile outcome — hash, commit and warnings on
// success, the error verbatim on failure.
func (s *Store) MarkVolumeSourceSynced(ctx context.Context, id, hash, commit, warnings string, syncErr error) error {
	status, errText := "ok", ""
	if syncErr != nil {
		status, errText = "error", syncErr.Error()
	}
	_, err := s.exec(ctx, `UPDATE volume_sources SET synced_hash = ?, synced_commit = ?,
        synced_at = ?, status = ?, last_error = ?, warnings = ? WHERE id = ?`,
		hash, commit, ts(now()), status, errText, warnings, id)
	return err
}

func (s *Store) DeleteVolumeSource(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM volume_sources WHERE id = ?`, id)
	return err
}

// ── inline source files ───────────────────────────────────────────────────────────

// VolSourceFile is one file an inline volume source delivers into its volume. Plaintext on
// purpose: the whole point is that it is viewed and edited in the console; sealed material
// belongs in stack secrets. Path is the file's name in the volume (may contain slashes for
// subdirectories, e.g. dynamic/middlewares.yml).
type VolSourceFile struct {
	Path    string
	Content string
	Mode    int64
}

// SetVolSourceFiles replaces an inline source's files wholesale, like SetStackEnv: the UI
// edits the set, and a partial update would strand a deleted file in the row set (though not
// yet in the volume — see the sync, which wipes before it writes).
func (s *Store) SetVolSourceFiles(ctx context.Context, sourceID string, files []VolSourceFile) error {
	defer s.lockWrites()()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: setting volume-source files: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM volsource_files WHERE source_id = ?`), sourceID); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO volsource_files (source_id, path, content, mode) VALUES (?, ?, ?, ?)`),
			sourceID, f.Path, f.Content, f.Mode); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// VolSourceFiles returns an inline source's files, ordered by path so the hash and the UI
// are stable.
func (s *Store) VolSourceFiles(ctx context.Context, sourceID string) ([]VolSourceFile, error) {
	rows, err := s.query(ctx, `SELECT path, content, mode FROM volsource_files
        WHERE source_id = ? ORDER BY path`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("store: reading volume-source files: %w", err)
	}
	defer rows.Close()

	var out []VolSourceFile
	for rows.Next() {
		var f VolSourceFile
		if err := rows.Scan(&f.Path, &f.Content, &f.Mode); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// VolumeBackupJobNames returns the enabled volume backup jobs guarding a named volume on
// one host. It is the other half of the volume-delete guard: a volume a job still backs
// up is a volume somebody declared precious.
func (s *Store) VolumeBackupJobNames(ctx context.Context, envID, volume string) ([]string, error) {
	rows, err := s.query(ctx, `SELECT name FROM backup_jobs
        WHERE env_id = ? AND engine = 'volume' AND volume = ? AND enabled = 1 ORDER BY name`,
		envID, volume)
	if err != nil {
		return nil, fmt.Errorf("store: listing volume backup jobs: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
