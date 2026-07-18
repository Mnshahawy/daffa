package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type BackupJob struct {
	ID            string
	EnvID         string
	Name          string
	Container     string
	Engine        string // postgres | mysql | mongodb | volume
	Databases     string // empty = everything
	DBUser        string
	DBPasswordEnc string
	Schedule      string // cron; empty = manual only

	// The volume engine's subject (container is unused there). A file-level snapshot of a
	// live database is torn, so StopContainers names what to stop for the duration —
	// downtime traded for consistency, per job, in writing. See docs/volumes.md.
	Volume         string
	StopContainers string // space-separated; empty = snapshot live
	// ExcludePaths are paths (relative to the volume root) dropped from the snapshot —
	// regenerable junk not worth backing up. Newline-separated; empty = snapshot everything.
	// A directory pattern drops its whole subtree. See docs/volumes.md.
	ExcludePaths string

	// Where the snapshots go. The bucket and its credentials live on the storage target
	// (shared between jobs); only the path within it belongs to this job.
	StorageID string
	Prefix    string

	Encryption string // age | none
	// KeyIDs are the encryption keys (public age recipients) this job encrypts to,
	// via backup_job_keys. Loaded alongside the row, stored separately from it.
	KeyIDs []string

	Enabled   bool
	CreatedAt time.Time
	CreatedBy string
}

// Encrypted reports whether this job's snapshots are readable by anyone holding the
// bucket. The distinction matters enough to be a method rather than a string compare
// scattered around the codebase.
func (j *BackupJob) Encrypted() bool { return j.Encryption == "age" }

const jobCols = `id, env_id, name, container, engine, databases, db_user, db_password_enc,
    schedule, volume, stop_containers, exclude_paths, storage_id, prefix, encryption, enabled, created_at, created_by`

func scanJob(sc interface{ Scan(...any) error }) (*BackupJob, error) {
	var j BackupJob
	var createdBy sql.NullString
	var createdAt string
	var enabled int
	err := sc.Scan(&j.ID, &j.EnvID, &j.Name, &j.Container, &j.Engine, &j.Databases, &j.DBUser,
		&j.DBPasswordEnc, &j.Schedule, &j.Volume, &j.StopContainers, &j.ExcludePaths, &j.StorageID, &j.Prefix, &j.Encryption,
		&enabled, &createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled != 0
	j.CreatedBy = createdBy.String
	j.CreatedAt = parseTS(createdAt)
	return &j, nil
}

func (s *Store) CreateBackupJob(ctx context.Context, j *BackupJob) error {
	if j.ID == "" {
		j.ID = NewID()
	}
	j.CreatedAt = now()

	// The row and its keys go in together, or neither does. A committed job row whose keys
	// failed to attach is a job set to encrypt to FEWER recipients than asked for — the exact
	// "silently narrows who can restore" hazard that no-ON-DELETE on backup_job_keys guards.
	defer s.lockWrites()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: creating backup job: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO backup_jobs (`+jobCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		j.ID, j.EnvID, j.Name, j.Container, j.Engine, j.Databases, j.DBUser, j.DBPasswordEnc,
		j.Schedule, j.Volume, j.StopContainers, j.ExcludePaths, j.StorageID, j.Prefix, j.Encryption,
		boolInt(j.Enabled), ts(j.CreatedAt), nullStr(j.CreatedBy)); err != nil {
		return fmt.Errorf("store: creating backup job: %w", err)
	}
	if err := setBackupJobKeysTx(ctx, s, tx, j.ID, j.KeyIDs); err != nil {
		return err
	}
	return tx.Commit()
}

// SetBackupJobKeys replaces the set of encryption keys a job encrypts to.
//
// Wholesale replace inside one transaction: a DELETE followed by a loop of auto-committed
// INSERTs could fail partway and leave the job with fewer recipients than intended, silently
// narrowing who can decrypt its backups.
func (s *Store) SetBackupJobKeys(ctx context.Context, jobID string, keyIDs []string) error {
	defer s.lockWrites()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: setting job keys: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := setBackupJobKeysTx(ctx, s, tx, jobID, keyIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func setBackupJobKeysTx(ctx context.Context, s *Store, tx *sql.Tx, jobID string, keyIDs []string) error {
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM backup_job_keys WHERE job_id = ?`), jobID); err != nil {
		return fmt.Errorf("store: clearing job keys: %w", err)
	}
	for _, keyID := range keyIDs {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO backup_job_keys (job_id, key_id) VALUES (?, ?)`),
			jobID, keyID); err != nil {
			return fmt.Errorf("store: linking job to key %s: %w", keyID, err)
		}
	}
	return nil
}

// JobRecipients resolves a job's keys to the age public keys the pipeline encrypts to.
// This is the seam that keeps backups ignorant of key management: the pipeline still just
// receives recipient strings.
func (s *Store) JobRecipients(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.query(ctx, `SELECT k.recipient FROM backup_job_keys jk
        JOIN encryption_keys k ON k.id = jk.key_id WHERE jk.job_id = ? ORDER BY k.name`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: resolving job recipients: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) BackupJobByID(ctx context.Context, id string) (*BackupJob, error) {
	j, err := scanJob(s.queryRow(ctx, `SELECT `+jobCols+` FROM backup_jobs WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.attachJobKeys(ctx, []*BackupJob{j}); err != nil {
		return nil, err
	}
	return j, nil
}

// attachJobKeys loads KeyIDs for a batch of jobs in one query, not one per job.
func (s *Store) attachJobKeys(ctx context.Context, jobs []*BackupJob) error {
	if len(jobs) == 0 {
		return nil
	}
	byID := make(map[string]*BackupJob, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
	}
	rows, err := s.query(ctx, `SELECT jk.job_id, jk.key_id FROM backup_job_keys jk
        JOIN encryption_keys k ON k.id = jk.key_id ORDER BY k.name`)
	if err != nil {
		return fmt.Errorf("store: loading job keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, keyID string
		if err := rows.Scan(&jobID, &keyID); err != nil {
			return err
		}
		if j, ok := byID[jobID]; ok {
			j.KeyIDs = append(j.KeyIDs, keyID)
		}
	}
	return rows.Err()
}

// AllBackupJobs returns EVERY job, ignoring permissions. It is for the scheduler, which
// runs on behalf of the system and not of any user. Never call it from a request handler:
// use ListBackupJobs, which filters.
func (s *Store) AllBackupJobs(ctx context.Context) ([]*BackupJob, error) {
	return s.ListBackupJobs(ctx, true, nil)
}

// ListBackupJobs returns the jobs on hosts the caller may see.
func (s *Store) ListBackupJobs(ctx context.Context, global bool, envs []string) ([]*BackupJob, error) {
	where, args := envIn(global, envs)
	if where == neverMatches {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT `+jobCols+` FROM backup_jobs`+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing backup jobs: %w", err)
	}
	defer rows.Close()

	var out []*BackupJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := s.attachJobKeys(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) SetBackupJobEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := s.exec(ctx, `UPDATE backup_jobs SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
	return err
}

func (s *Store) DeleteBackupJob(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM backup_jobs WHERE id = ?`, id)
	return err
}

// ── runs ────────────────────────────────────────────────────────────────────────

type BackupRun struct {
	ID        string
	JobID     string
	Status    string // running | ok | failed
	Trigger   string // manual | schedule
	Bytes     int64
	ObjectKey string
	Error     string
	StartedAt time.Time
	EndedAt   time.Time
	StartedBy string
}

func (s *Store) StartBackupRun(ctx context.Context, run *BackupRun) error {
	if run.ID == "" {
		run.ID = NewID()
	}
	run.Status = "running"
	run.StartedAt = now()
	_, err := s.exec(ctx, `INSERT INTO backup_runs (id, job_id, status, trigger, bytes, object_key,
        error, started_at, ended_at, started_by)
        VALUES (?, ?, ?, ?, 0, '', '', ?, NULL, ?)`,
		run.ID, run.JobID, run.Status, run.Trigger, ts(run.StartedAt), nullStr(run.StartedBy))
	if err != nil {
		return fmt.Errorf("store: starting backup run: %w", err)
	}
	return nil
}

// FinishBackupRun records the outcome. A failed backup is not a footnote — it is the
// single most important thing this feature has to tell anyone, so it is stored with the
// error text rather than just a status.
func (s *Store) FinishBackupRun(ctx context.Context, runID string, bytes int64, objectKey string, runErr error) error {
	status, errText := "ok", ""
	if runErr != nil {
		status, errText = "failed", runErr.Error()
	}
	_, err := s.exec(ctx, `UPDATE backup_runs SET status = ?, bytes = ?, object_key = ?, error = ?,
        ended_at = ? WHERE id = ?`,
		status, bytes, objectKey, errText, ts(now()), runID)
	return err
}

func (s *Store) ListBackupRuns(ctx context.Context, jobID string, limit int) ([]*BackupRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.query(ctx, `SELECT id, job_id, status, trigger, bytes, object_key, error,
        started_at, ended_at, started_by FROM backup_runs WHERE job_id = ?
        ORDER BY started_at DESC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing backup runs: %w", err)
	}
	defer rows.Close()

	var out []*BackupRun
	for rows.Next() {
		var r BackupRun
		var endedAt, startedBy sql.NullString
		var startedAt string
		if err := rows.Scan(&r.ID, &r.JobID, &r.Status, &r.Trigger, &r.Bytes, &r.ObjectKey,
			&r.Error, &startedAt, &endedAt, &startedBy); err != nil {
			return nil, err
		}
		r.StartedAt = parseTS(startedAt)
		r.StartedBy = startedBy.String
		if endedAt.Valid {
			r.EndedAt = parseTS(endedAt.String)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// LastBackupRun is what the dashboard shows: a red last-run is the signal that matters.
func (s *Store) LastBackupRun(ctx context.Context, jobID string) (*BackupRun, error) {
	runs, err := s.ListBackupRuns(ctx, jobID, 1)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, ErrNotFound
	}
	return runs[0], nil
}

// FailStaleBackupRuns marks runs that were in flight when the process died. A backup
// stuck at "running" forever would look like it is still working, which is the most
// dangerous lie this system could tell.
func (s *Store) FailStaleBackupRuns(ctx context.Context) error {
	_, err := s.exec(ctx, `UPDATE backup_runs SET status = 'failed',
        error = 'the server stopped while this backup was running', ended_at = ?
        WHERE status = 'running'`, ts(now()))
	return err
}
