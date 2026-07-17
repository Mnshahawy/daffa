package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/store"
	"github.com/Mnshahawy/daffa/internal/tunnel"
)

// Reconciliation is how Daffa learns what a Swarm is, and the rule it obeys is short:
//
//	THE DAEMON IS AUTHORITATIVE. THESE ROWS ARE A CACHE.
//
// Nothing here decides anything. It asks each daemon what it is — Info().Swarm, on a call the
// liveness ping was already making — and makes the rows agree with the answer. Membership is
// therefore DISCOVERED, never asserted: there is no "add this node to that cluster" control
// anywhere in Daffa, because such a control could only ever disagree with the Swarm, and it would
// lose. The Swarm schedules tasks onto the node regardless of what a table believes.
//
// The one thing it will not do is MERGE. See reconcileNode's last case.

// ReconcileAll asks every connected daemon what it is. Called once at startup, before the server
// answers anything, and again on every liveness sweep.
func (s *Server) ReconcileAll(ctx context.Context) {
	envs, err := s.store.ListEnvironments(ctx)
	if err != nil {
		slog.Error("reconciling swarms: listing environments", "err", err)
		return
	}
	for _, e := range envs {
		s.reconcileEnv(ctx, e.ID)
	}
}

// reconcileEnv re-reads every node in one environment and settles what that environment is.
func (s *Server) reconcileEnv(ctx context.Context, envID string) {
	env, err := s.pool.Get(envID)
	if err != nil {
		return // not connected; nothing to ask
	}
	for _, node := range env.Nodes() {
		s.reconcileNode(ctx, envID, node)
	}
}

// reconcileNode asks one daemon what it is, and applies the assembly rules (docs/swarm.md §2).
func (s *Server) reconcileNode(ctx context.Context, envID string, node *dockerx.Node) {
	if node == nil {
		return
	}
	info, err := node.Swarm(ctx)
	if err != nil {
		return // the daemon is unreachable; say nothing, watchHosts owns "this is down"
	}

	row, err := s.store.NodeByID(ctx, node.ID)
	if err != nil {
		return
	}

	// ── the node's own swarm identity ────────────────────────────────────────────
	role := "none"
	switch {
	case info.Manager:
		role = "manager"
	case info.InSwarm:
		role = "worker"
	}

	if row.SwarmNodeID != info.NodeID || row.SwarmRole != role || row.IsLeader != info.Leader {
		if err := s.store.SetNodeSwarm(ctx, node.ID, info.NodeID, role, info.Leader); err != nil {
			slog.Error("reconciling a node's swarm role", "node", node.Name, "err", err)
			return
		}
		// A machine that silently stopped being a manager is something an operator should find in
		// the audit log, not in an error message at 2am.
		if row.SwarmRole != role {
			s.audit(ctx, store.AuditEntry{
				EnvID: envID, Action: "swarm.node.role", Target: node.Name, Outcome: "ok",
				Detail: store.AuditDetail(map[string]string{"from": row.SwarmRole, "to": role}),
			})
		}
		s.pool.SetSwarm(envID, node.ID, info.ClusterID, info.NodeID, role, info.Leader)
	}

	// ── which environment this node belongs in ───────────────────────────────────
	env, err := s.store.EnvironmentByID(ctx, envID)
	if err != nil {
		return
	}

	// The daemon is not in a swarm. If we thought its environment was one, it no longer is.
	if !info.InSwarm {
		if env.SwarmID != "" && len(s.nodesIn(ctx, envID)) == 1 {
			s.setSwarm(ctx, env, "", node.Name)
		}
		return
	}

	// The daemon IS in a swarm, and its environment already agrees. Nothing to do.
	if env.SwarmID == info.ClusterID {
		return
	}

	owner, err := s.store.EnvironmentBySwarm(ctx, info.ClusterID)
	switch {
	// Nobody else claims this swarm. This environment becomes it, IN PLACE, keeping its id — so
	// its stacks, its backup jobs and every grant on it survive untouched. It is the same
	// environment; it has simply grown.
	case errors.Is(err, store.ErrNotFound):
		s.setSwarm(ctx, env, info.ClusterID, node.Name)

	case err != nil:
		slog.Error("reconciling a swarm", "env", env.Name, "err", err)

	// Another environment already IS this swarm, and this node is somewhere else.
	default:
		nodes := s.nodesIn(ctx, envID)

		// If this node is alone in its environment, and that environment has nothing hanging off
		// it, the node simply joins the swarm's environment. This is the freshly-enrolled case:
		// nothing to merge, so nothing can be silently merged.
		if len(nodes) == 1 && s.envIsBare(ctx, env.ID) {
			if err := s.store.MoveNode(ctx, node.ID, owner.ID); err != nil {
				slog.Error("attaching a node to its swarm", "node", node.Name, "err", err)
				return
			}
			s.pool.Deregister(env.ID, node.ID)
			s.reconnectNode(ctx, owner, node.ID)
			slog.Info("node joined its swarm", "node", node.Name, "environment", owner.Name)
			s.audit(ctx, store.AuditEntry{
				EnvID: owner.ID, Action: "swarm.node.join", Target: node.Name, Outcome: "ok",
				Detail: store.AuditDetail(map[string]string{"swarm": info.ClusterID}),
			})
			return
		}

		// TWO ENVIRONMENTS, ONE SWARM. Daffa does not merge them.
		//
		// An environment is what a grant points at. If `staging` and `prod` each have their own
		// stacks and their own grants, and somebody joins both to one Swarm, then merging them
		// would silently merge two sets of permissions — an authorization change caused by an
		// action taken OUTSIDE Daffa, which is the worst way for one to happen.
		//
		// So it refuses, loudly, keeps operating both exactly as they are, and makes a person
		// decide. Refusing is not a limitation here; it is the feature.
		slog.Warn("two environments are the same swarm — refusing to merge them",
			"swarm", info.ClusterID, "a", env.Name, "b", owner.Name)
		s.audit(ctx, store.AuditEntry{
			EnvID: env.ID, Action: "swarm.conflict", Target: env.Name, Outcome: "error",
			Detail: store.AuditDetail(map[string]string{
				"swarm": info.ClusterID,
				"other": owner.Name,
				"why": "Two environments report the same Swarm. Daffa will not merge them: their " +
					"stacks and their grants differ, and merging would silently merge both. " +
					"Remove one of them.",
			}),
		})
	}
}

func (s *Server) setSwarm(ctx context.Context, env *store.Environment, swarmID, nodeName string) {
	if err := s.store.SetEnvironmentSwarm(ctx, env.ID, swarmID); err != nil {
		slog.Error("recording an environment's swarm", "env", env.Name, "err", err)
		return
	}
	s.pool.SetEnvSwarm(env.ID, swarmID)

	action, detail := "swarm.joined", map[string]string{"swarm": swarmID}
	if swarmID == "" {
		action, detail = "swarm.left", map[string]string{}
	}
	slog.Info("environment swarm changed", "env", env.Name, "swarm", swarmID)
	s.audit(ctx, store.AuditEntry{
		EnvID: env.ID, Action: action, Target: env.Name, Outcome: "ok",
		Detail: store.AuditDetail(detail),
	})
}

func (s *Server) nodesIn(ctx context.Context, envID string) []*store.Node {
	nodes, err := s.store.NodesByEnv(ctx, envID)
	if err != nil {
		return nil
	}
	return nodes
}

// envIsBare reports whether an environment has nothing hanging off it — no stacks, no backup jobs,
// no grants. Only a bare environment may be dissolved by moving its node into a swarm; anything
// else is somebody's configuration, and it does not get moved without them.
func (s *Server) envIsBare(ctx context.Context, envID string) bool {
	stacks, err := s.store.ListStacks(ctx, false, []string{envID})
	if err != nil || len(stacks) > 0 {
		return false
	}
	jobs, err := s.store.ListBackupJobs(ctx, false, []string{envID})
	if err != nil || len(jobs) > 0 {
		return false
	}
	granted, err := s.store.EnvHasGrants(ctx, envID)
	if err != nil || granted {
		return false
	}
	return true
}

// reconnectNode re-registers a moved node's live client under its new environment. The tunnel is
// unchanged — only which environment the pool files it under.
func (s *Server) reconnectNode(ctx context.Context, env *store.Environment, nodeID string) {
	node, err := s.store.NodeByID(ctx, nodeID)
	if err != nil {
		return
	}
	if node.AgentID == "" {
		return
	}
	sess, ok := s.agents.get(node.AgentID)
	if !ok {
		return
	}
	if err := s.pool.RegisterAgent(env, node, tunnel.Dialer(sess)); err != nil {
		slog.Error("re-registering a moved node", "node", node.Name, "err", err)
	}
}
