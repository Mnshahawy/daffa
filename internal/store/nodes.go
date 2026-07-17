package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Node is one Docker daemon — one machine. It is what an environment is made OF, and it is not a
// thing a person selects or scopes a grant to. The `local | agent` distinction lives here rather
// than on the environment because it was always a property of a daemon: how Daffa dials it.
//
// The swarm columns are reconciled FROM the daemon (Info().Swarm) and are never the authority on
// anything — see docs/swarm.md §2. SwarmNodeID is the join key that turns a task's NodeID into
// the client that can exec into it.
type Node struct {
	ID         string
	EnvID      string
	Name       string
	Kind       string // local | agent
	DockerHost string
	AgentID    string

	SwarmNodeID string
	SwarmRole   string // none | worker | manager
	IsLeader    bool

	Status     string
	LastSeenAt time.Time
}

// Manager reports whether this daemon will answer cluster-wide questions. It is the only bit that
// matters when picking a control node: not "is it labelled a manager" but "will this socket
// answer me", which is exactly what Docker's Swarm.ControlAvailable means.
func (n *Node) Manager() bool { return n.SwarmRole == "manager" }

const nodeCols = `id, env_id, name, kind, docker_host, agent_id,
    swarm_node_id, swarm_role, is_leader, status, last_seen_at`

func scanNode(sc interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var agentID, lastSeen sql.NullString
	var isLeader int
	if err := sc.Scan(&n.ID, &n.EnvID, &n.Name, &n.Kind, &n.DockerHost, &agentID,
		&n.SwarmNodeID, &n.SwarmRole, &isLeader, &n.Status, &lastSeen); err != nil {
		return nil, err
	}
	n.AgentID = agentID.String
	n.IsLeader = isLeader != 0
	if lastSeen.Valid {
		n.LastSeenAt = parseTS(lastSeen.String)
	}
	return &n, nil
}

func (s *Store) CreateNode(ctx context.Context, n *Node) error {
	if n.ID == "" {
		n.ID = NewID()
	}
	if n.Status == "" {
		n.Status = "unknown"
	}
	if n.SwarmRole == "" {
		n.SwarmRole = "none"
	}
	_, err := s.exec(ctx, `INSERT INTO nodes (`+nodeCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.EnvID, n.Name, n.Kind, n.DockerHost, nullStr(n.AgentID),
		n.SwarmNodeID, n.SwarmRole, boolInt(n.IsLeader), n.Status, nullTS(n.LastSeenAt))
	if err != nil {
		return fmt.Errorf("store: creating node: %w", err)
	}
	return nil
}

func (s *Store) NodeByID(ctx context.Context, id string) (*Node, error) {
	n, err := scanNode(s.queryRow(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

func (s *Store) NodeByAgent(ctx context.Context, agentID string) (*Node, error) {
	n, err := scanNode(s.queryRow(ctx, `SELECT `+nodeCols+` FROM nodes WHERE agent_id = ?`, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// localNode finds the node that is the local Docker socket. There is at most one.
func (s *Store) localNode(ctx context.Context) (*Node, error) {
	n, err := scanNode(s.queryRow(ctx, `SELECT `+nodeCols+` FROM nodes WHERE kind = 'local'`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// ListNodes returns every node, in every environment. The pool wants all of them at startup, and
// the switcher wants them grouped; neither wants a query per environment.
func (s *Store) ListNodes(ctx context.Context) ([]*Node, error) {
	return s.nodesWhere(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY env_id, name`)
}

func (s *Store) NodesByEnv(ctx context.Context, envID string) ([]*Node, error) {
	return s.nodesWhere(ctx, `SELECT `+nodeCols+` FROM nodes WHERE env_id = ? ORDER BY name`, envID)
}

func (s *Store) nodesWhere(ctx context.Context, q string, args ...any) ([]*Node, error) {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing nodes: %w", err)
	}
	defer rows.Close()

	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpsertAgentNode creates (or finds) the node an agent stands for, and the standalone environment
// that holds it. It is called when the agent CONNECTS, so a node never exists for a machine that
// has never checked in.
//
// A newly enrolled node always lands in its own standalone environment. Swarm assembly happens
// afterwards, from what the daemon reports about itself (docs/swarm.md §2) — never from what the
// enrolling side asserted, because the daemon is the only thing that actually knows.
func (s *Store) UpsertAgentNode(ctx context.Context, agentID, name string) (*Environment, *Node, error) {
	node, err := s.NodeByAgent(ctx, agentID)
	switch {
	case err == nil:
		env, err := s.EnvironmentByID(ctx, node.EnvID)
		if err != nil {
			return nil, nil, err
		}
		return env, node, nil

	case errors.Is(err, ErrNotFound):
		env := &Environment{ID: NewID(), Name: name, Status: "online", CreatedAt: now()}
		if err := s.CreateEnvironment(ctx, env); err != nil {
			return nil, nil, err
		}
		node = &Node{ID: NewID(), EnvID: env.ID, Name: name, Kind: "agent", AgentID: agentID, Status: "online"}
		if err := s.CreateNode(ctx, node); err != nil {
			return nil, nil, err
		}
		return env, node, nil

	default:
		return nil, nil, err
	}
}

// SetNodeSwarm records what the daemon said about itself. The daemon is authoritative and this row
// is a cache: when they disagree, this is the function that makes the row agree, and the caller
// audits the correction.
func (s *Store) SetNodeSwarm(ctx context.Context, nodeID, swarmNodeID, role string, leader bool) error {
	_, err := s.exec(ctx, `UPDATE nodes SET swarm_node_id = ?, swarm_role = ?, is_leader = ? WHERE id = ?`,
		swarmNodeID, role, boolInt(leader), nodeID)
	return err
}

// EnvironmentBySwarm finds the environment that IS a given swarm, if Daffa knows one.
func (s *Store) EnvironmentBySwarm(ctx context.Context, swarmID string) (*Environment, error) {
	if swarmID == "" {
		return nil, ErrNotFound
	}
	e, err := scanEnv(s.queryRow(ctx, `SELECT `+envCols+` FROM environments WHERE swarm_id = ?`, swarmID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// SetEnvironmentSwarm marks an environment as being a particular swarm — or, with "", as being
// standalone again because its daemon left one.
func (s *Store) SetEnvironmentSwarm(ctx context.Context, envID, swarmID string) error {
	_, err := s.exec(ctx, `UPDATE environments SET swarm_id = ? WHERE id = ?`, swarmID, envID)
	if err != nil {
		return fmt.Errorf("store: setting the swarm on an environment: %w", err)
	}
	return nil
}

// MoveNode attaches a node to a different environment, and deletes the one it left if that empties
// it. This is how a freshly enrolled node joins the environment that already IS its swarm.
//
// It is only ever called for a node with nothing hanging off it — see the assembly rules in
// docs/swarm.md §2. Moving a node that an environment's stacks or grants referred to would be
// moving those too, silently, which is the thing the rules exist to forbid.
func (s *Store) MoveNode(ctx context.Context, nodeID, toEnvID string) error {
	node, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		return err
	}
	from := node.EnvID
	if from == toEnvID {
		return nil
	}

	if _, err := s.exec(ctx, `UPDATE nodes SET env_id = ? WHERE id = ?`, toEnvID, nodeID); err != nil {
		return fmt.Errorf("store: moving a node: %w", err)
	}

	rest, err := s.NodesByEnv(ctx, from)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return nil
	}
	_, err = s.exec(ctx, `DELETE FROM environments WHERE id = ?`, from)
	return err
}

func (s *Store) SetNodeStatus(ctx context.Context, nodeID, status string) error {
	_, err := s.exec(ctx, `UPDATE nodes SET status = ?, last_seen_at = ? WHERE id = ?`,
		status, ts(now()), nodeID)
	return err
}

// DeleteNodeByAgent removes the node an agent stood for, and — if that leaves its environment with
// no nodes at all — the environment too.
//
// The empty environment must go, and not only for tidiness: it is the thing grants are scoped to.
// An environment that still exists but can reach nothing is a grant that still exists and confers
// nothing visible, waiting for a node to be enrolled into it and silently mean something again.
func (s *Store) DeleteNodeByAgent(ctx context.Context, agentID string) error {
	node, err := s.NodeByAgent(ctx, agentID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	if _, err := s.exec(ctx, `DELETE FROM nodes WHERE id = ?`, node.ID); err != nil {
		return fmt.Errorf("store: deleting node: %w", err)
	}

	rest, err := s.NodesByEnv(ctx, node.EnvID)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return nil
	}

	_, err = s.exec(ctx, `DELETE FROM environments WHERE id = ?`, node.EnvID)
	return err
}

func nullTS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return ts(t)
}
