package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GitCredential is how Daffa authenticates to a git server: an access token over HTTPS,
// or a private key over SSH.
type GitCredential struct {
	ID       string
	Name     string
	Kind     string // token | ssh
	Username string
	TokenEnc string
	// SSHKeyID references a key in the shared SSH-key store (ssh_keys) for kind == "ssh". A git
	// credential carries no key material of its own — that management moved to the key store.
	SSHKeyID string
	// HostKey pins the server's SSH host key (one line of ssh-keyscan output). Empty
	// means unpinned — which works, but means a substituted server would be trusted.
	HostKey   string
	CreatedAt time.Time
	CreatedBy string
}

const (
	GitToken = "token"
	GitSSH   = "ssh"
)

const gitCredCols = `id, name, kind, username, token_enc, ssh_key_id,
    host_key, created_at, created_by`

func scanGitCred(sc interface{ Scan(...any) error }) (*GitCredential, error) {
	var c GitCredential
	var createdBy sql.NullString
	var createdAt string
	err := sc.Scan(&c.ID, &c.Name, &c.Kind, &c.Username, &c.TokenEnc, &c.SSHKeyID,
		&c.HostKey, &createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedBy = createdBy.String
	c.CreatedAt = parseTS(createdAt)
	return &c, nil
}

func (s *Store) CreateGitCredential(ctx context.Context, c *GitCredential) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	c.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO git_credentials (`+gitCredCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.Kind, c.Username, c.TokenEnc, c.SSHKeyID,
		c.HostKey, ts(c.CreatedAt), nullStr(c.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating git credential: %w", err)
	}
	return nil
}

func (s *Store) GitCredentialByID(ctx context.Context, id string) (*GitCredential, error) {
	return scanGitCred(s.queryRow(ctx, `SELECT `+gitCredCols+` FROM git_credentials WHERE id = ?`, id))
}

func (s *Store) ListGitCredentials(ctx context.Context) ([]*GitCredential, error) {
	rows, err := s.query(ctx, `SELECT `+gitCredCols+` FROM git_credentials ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing git credentials: %w", err)
	}
	defer rows.Close()

	var out []*GitCredential
	for rows.Next() {
		c, err := scanGitCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GitCredentialInUse counts everything that depends on this credential, so it can be
// refused rather than deleted out from under a dependent that will next fail.
//
// Both stacks AND volume sources reference a git credential (volume_sources.git_credential_id),
// and neither FK carries an ON DELETE. Counting only stacks left the volume-source case to be
// caught by the raw FK violation — a 500 with a constraint message instead of the friendly 409
// this guard exists to give. Both are counted here so the refusal names the fix.
func (s *Store) GitCredentialInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT
        (SELECT COUNT(*) FROM stacks WHERE git_credential_id = ?) +
        (SELECT COUNT(*) FROM volume_sources WHERE git_credential_id = ?)`, id, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteGitCredential(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM git_credentials WHERE id = ?`, id)
	return err
}
