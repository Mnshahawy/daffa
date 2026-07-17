package dockerx

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/swarm"
)

// Swarm secrets and configs were once managed here, as free-floating cluster objects. They are
// retired: a secret is now something a STACK owns, sealed at rest and delivered as a bundle file
// that works on Compose as well as Swarm (docs/secrets.md), and config lives in git, delivered by
// a volume source (docs/volumes.md). What remains below is the cluster's own existence, which is
// nobody's stack.

// ── the cluster's own existence ─────────────────────────────────────────────────

// JoinTokens are CREDENTIALS. Anybody holding one can add a machine to the cluster, and a machine in
// the cluster runs whatever the cluster schedules onto it.
//
// Docker returns them from GET /swarm, alongside a lot of harmless information — so anything that
// inspects a swarm and hands the result to a browser is one careless line away from leaking them.
// Portainer strips JoinTokens and TLSInfo from that response for non-admins; we never put them in a
// shared payload at all. They are returned by ONE route, which requires swarm.edit and nothing else.
type JoinTokens struct {
	Worker  string `json:"worker"`
	Manager string `json:"manager"`
	// Addr is the address a joining machine dials — the manager's advertised address, which is what
	// the join command needs and is not something an operator should have to go and find.
	Addr string `json:"addr"`
}

// SwarmInit turns a standalone daemon into a single-node Swarm, of which it is the manager.
//
// This is the one Swarm operation that runs against a daemon that is NOT yet a manager — it is what
// makes it one — so it is the one place s.control() cannot be used to find its target.
func (e *Node) SwarmInit(ctx context.Context, advertiseAddr string) (string, error) {
	req := swarm.InitRequest{
		ListenAddr: "0.0.0.0:2377",
		// Empty means "work it out yourself", which Docker does correctly on a machine with one
		// obvious address and refuses to guess on a machine with several. Refusing to guess is the
		// right behaviour and the error says so, so we pass it straight through rather than picking
		// an interface on the operator's behalf and being wrong on the machine that matters.
		AdvertiseAddr: advertiseAddr,
	}
	return e.Client.SwarmInit(ctx, req)
}

// SwarmJoinTokens reads the credentials that let a machine join.
func (e *Node) SwarmJoinTokens(ctx context.Context) (*JoinTokens, error) {
	sw, err := e.Client.SwarmInspect(ctx)
	if err != nil {
		return nil, err
	}

	info, err := e.Swarm(ctx)
	if err != nil {
		return nil, err
	}
	if !info.Manager {
		return nil, fmt.Errorf("dockerx: only a manager holds the join tokens")
	}

	addr := ""
	if nodes, err := e.ListSwarmNodes(ctx); err == nil {
		for _, n := range nodes {
			if n.ID == info.NodeID && n.Addr != "" {
				addr = n.Addr + ":2377"
				break
			}
		}
	}

	return &JoinTokens{
		Worker:  sw.JoinTokens.Worker,
		Manager: sw.JoinTokens.Manager,
		Addr:    addr,
	}, nil
}

// SwarmLeave takes THIS daemon out of the swarm.
//
// force is required for the last manager, because leaving with it dissolves the cluster: the raft
// store goes, and with it every service, secret and config definition. The services' containers keep
// running until something stops them, which makes the damage quiet as well as total.
func (e *Node) SwarmLeave(ctx context.Context, force bool) error {
	return e.Client.SwarmLeave(ctx, force)
}
