package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Agent struct {
	ID         string
	Name       string
	TokenHash  string // empty until the agent enrolls
	Version    string
	LastSeenAt time.Time
	CreatedAt  time.Time
	CreatedBy  string
}

// Enrolled reports whether the agent has ever exchanged its join token for a real one.
func (a *Agent) Enrolled() bool { return a.TokenHash != "" }

const agentCols = `id, name, token_hash, version, last_seen_at, created_at, created_by`

func scanAgent(sc interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var tokenHash, lastSeen, createdBy sql.NullString
	var createdAt string
	if err := sc.Scan(&a.ID, &a.Name, &tokenHash, &a.Version, &lastSeen, &createdAt, &createdBy); err != nil {
		return nil, err
	}
	a.TokenHash, a.CreatedBy = tokenHash.String, createdBy.String
	a.CreatedAt = parseTS(createdAt)
	if lastSeen.Valid {
		a.LastSeenAt = parseTS(lastSeen.String)
	}
	return &a, nil
}

func (s *Store) CreateAgent(ctx context.Context, a *Agent) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now()
	}
	_, err := s.exec(ctx, `INSERT INTO agents (`+agentCols+`)
        VALUES (?, ?, NULL, '', NULL, ?, ?)`,
		a.ID, a.Name, ts(a.CreatedAt), nullStr(a.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating agent: %w", err)
	}
	return nil
}

func (s *Store) AgentByID(ctx context.Context, id string) (*Agent, error) {
	a, err := scanAgent(s.queryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// AgentByToken authenticates a connecting agent. The token is looked up by its hash, so
// a leaked database still cannot be used to impersonate an agent.
func (s *Store) AgentByToken(ctx context.Context, tokenHash string) (*Agent, error) {
	a, err := scanAgent(s.queryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func (s *Store) ListAgents(ctx context.Context) ([]*Agent, error) {
	rows, err := s.query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: listing agents: %w", err)
	}
	defer rows.Close()

	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAgentToken completes enrollment. Called once, when the agent redeems a join token.
func (s *Store) SetAgentToken(ctx context.Context, agentID, tokenHash string) error {
	_, err := s.exec(ctx, `UPDATE agents SET token_hash = ? WHERE id = ?`, tokenHash, agentID)
	return err
}

func (s *Store) TouchAgent(ctx context.Context, agentID, version string) error {
	_, err := s.exec(ctx, `UPDATE agents SET last_seen_at = ?, version = ? WHERE id = ?`,
		ts(now()), version, agentID)
	return err
}

// DeleteAgent removes an agent and, with it, the node it stood for.
//
// The grants scoped to that node's environment go too — but ONLY when removing the node empties
// the environment. Otherwise they dangle: an agent re-enrolled onto the same environment id would
// silently restore access that an administrator believes they revoked when they removed the
// machine.
//
// The "only when it empties" condition is what changes with clusters, and it is the honest rule:
// pulling one node out of a five-node swarm does not revoke anybody's access to that swarm,
// because the swarm is still there and the grant still names it.
func (s *Store) DeleteAgent(ctx context.Context, agentID string) error {
	node, err := s.NodeByAgent(ctx, agentID)
	switch {
	case err == nil:
		rest, err := s.NodesByEnv(ctx, node.EnvID)
		if err != nil {
			return err
		}
		if len(rest) <= 1 {
			if err := s.RevokeEnvGrants(ctx, node.EnvID); err != nil {
				return err
			}
		}
	case !errors.Is(err, ErrNotFound):
		return err
	}

	if err := s.DeleteNodeByAgent(ctx, agentID); err != nil {
		return err
	}

	_, err = s.exec(ctx, `DELETE FROM agents WHERE id = ?`, agentID)
	return err
}

// ── join tokens ─────────────────────────────────────────────────────────────────

func (s *Store) CreateJoinToken(ctx context.Context, tokenHash, agentID string, expiresAt time.Time) error {
	_, err := s.exec(ctx, `INSERT INTO join_tokens (id, agent_id, created_at, expires_at)
        VALUES (?, ?, ?, ?)`, tokenHash, agentID, ts(now()), ts(expiresAt))
	if err != nil {
		return fmt.Errorf("store: creating join token: %w", err)
	}
	return nil
}

// RedeemJoinToken consumes a join token and returns the agent it enrolls. The row is
// deleted whether or not it had expired, so a token is good for at most one attempt.
func (s *Store) RedeemJoinToken(ctx context.Context, tokenHash string) (*Agent, error) {
	defer s.lockWrites()()

	var agentID, expires string
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT agent_id, expires_at FROM join_tokens WHERE id = ?`), tokenHash).Scan(&agentID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading join token: %w", err)
	}

	if _, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM join_tokens WHERE id = ?`), tokenHash); err != nil {
		return nil, fmt.Errorf("store: consuming join token: %w", err)
	}
	if !parseTS(expires).After(now()) {
		return nil, ErrNotFound // expired, and now also gone
	}

	return scanAgent(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+agentCols+` FROM agents WHERE id = ?`), agentID))
}

func (s *Store) DeleteExpiredJoinTokens(ctx context.Context) error {
	_, err := s.exec(ctx, `DELETE FROM join_tokens WHERE expires_at < ?`, ts(now()))
	return err
}

// An agent enrolls a NODE, not an environment. See UpsertAgentNode and DeleteNodeByAgent in
// nodes.go — this file no longer knows what an environment is.
