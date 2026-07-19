package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SSHKey is a keypair Daffa uses to dial OUT to a machine over SSH: a remote cluster's
// manager, or a node reached without an agent (docs/clusters.md §6). Configured once and
// shared, like a registry or a git credential.
//
// PublicKey and Fingerprint are PUBLIC material, stored plaintext on purpose — the public
// key is meant to be copied into the target's authorized_keys. PrivateKeyEnc and
// PassphraseEnc hold the sealed private half; they are write-only through the API and never
// travel back to the client.
type SSHKey struct {
	ID            string
	Name          string
	Algo          string // ed25519 | rsa
	PublicKey     string // one authorized_keys line
	Fingerprint   string // SHA256:…
	PrivateKeyEnc string
	PassphraseEnc string
	CreatedAt     time.Time
	CreatedBy     string
}

const (
	SSHKeyEd25519 = "ed25519"
	SSHKeyRSA     = "rsa"
)

const sshKeyCols = `id, name, algo, public_key, fingerprint, private_key_enc, passphrase_enc,
    created_at, created_by`

func scanSSHKey(sc interface{ Scan(...any) error }) (*SSHKey, error) {
	var k SSHKey
	var createdBy sql.NullString
	var createdAt string
	err := sc.Scan(&k.ID, &k.Name, &k.Algo, &k.PublicKey, &k.Fingerprint,
		&k.PrivateKeyEnc, &k.PassphraseEnc, &createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.CreatedBy = createdBy.String
	k.CreatedAt = parseTS(createdAt)
	return &k, nil
}

func (s *Store) CreateSSHKey(ctx context.Context, k *SSHKey) error {
	if k.ID == "" {
		k.ID = "sshkey_" + NewID()
	}
	k.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO ssh_keys (`+sshKeyCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.Algo, k.PublicKey, k.Fingerprint, k.PrivateKeyEnc, k.PassphraseEnc,
		ts(k.CreatedAt), nullStr(k.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating ssh key: %w", err)
	}
	return nil
}

func (s *Store) SSHKeyByID(ctx context.Context, id string) (*SSHKey, error) {
	return scanSSHKey(s.queryRow(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys WHERE id = ?`, id))
}

func (s *Store) ListSSHKeys(ctx context.Context) ([]*SSHKey, error) {
	rows, err := s.query(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing ssh keys: %w", err)
	}
	defer rows.Close()

	var out []*SSHKey
	for rows.Next() {
		k, err := scanSSHKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SSHKeyInUse counts everything that authenticates with this key, so a key still in use is
// refused rather than deleted out from under a dependent that would next fail (docs/clusters.md §6).
// Both a node (ssh_key_id) and an SSH git credential (git_credentials.ssh_key_id) reference a key;
// counting only one would let the other's key be deleted and surface as a runtime failure instead
// of the friendly refusal the guard exists to give.
func (s *Store) SSHKeyInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT
        (SELECT COUNT(*) FROM nodes WHERE ssh_key_id = ?) +
        (SELECT COUNT(*) FROM git_credentials WHERE ssh_key_id = ?)`, id, id).Scan(&n)
	return n, err
}

func (s *Store) DeleteSSHKey(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM ssh_keys WHERE id = ?`, id)
	return err
}
