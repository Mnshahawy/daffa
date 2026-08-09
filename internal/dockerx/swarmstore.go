package dockerx

import (
	"context"
	"fmt"
	"net/http"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
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

// SwarmJoin makes THIS daemon join an existing swarm, as a worker or manager depending on the
// token. remoteAddr is a manager's advertised address (host:2377); advertiseAddr is the joining
// node's OWN reachable address.
//
// advertiseAddr is the whole point of orchestrating the join rather than telling an operator to run
// `docker swarm join` by hand: a node that advertises its private NIC black-holes the overlay
// (VXLAN cannot reach it), which is the failure we hit manually. Daffa sets the reachable address —
// the host it dialed to get here — at join time (docs/clusters.md §5). DataPathAddr rides with it,
// since the overlay data plane has the same reachability requirement as the control plane.
func (e *Node) SwarmJoin(ctx context.Context, remoteAddr, token, advertiseAddr string) error {
	return e.Client.SwarmJoin(ctx, swarm.JoinRequest{
		ListenAddr:    "0.0.0.0:2377",
		AdvertiseAddr: advertiseAddr,
		DataPathAddr:  advertiseAddr,
		RemoteAddrs:   []string{remoteAddr},
		JoinToken:     token,
	})
}

// SwarmJoinVia joins a daemon reached through dial to a swarm, without touching the pool — the new
// node is dialed over SSH just long enough to issue the join; its lasting connection is opened by
// the connect loop afterwards, the same way an added cluster's is.
func SwarmJoinVia(ctx context.Context, dial Dialer, remoteAddr, token, advertiseAddr string) error {
	c, err := client.NewClientWithOpts(
		client.WithHost("http://daffa-join"),
		client.WithHTTPClient(&http.Client{Transport: &http.Transport{
			DialContext:       dial,
			DisableKeepAlives: true,
		}}),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("dockerx: building join client: %w", err)
	}
	defer c.Close()
	return (&Node{Name: "join", Client: c}).SwarmJoin(ctx, remoteAddr, token, advertiseAddr)
}

// TaskHistoryLimit is how many finished tasks Swarm keeps per replica.
//
// It is a disk setting wearing orchestration clothes. Every kept task is a real stopped CONTAINER
// on whichever node ran it, writable layer and all — on a service that writes into its own
// filesystem that is gigabytes per dead task, and Docker's default of 5 means five deployments'
// worth. Lowering it reduces what accumulates; the scheduled cleanup removes what already has
// (.ai/cleanup.md). The two are complements, not alternatives.
//
// Docker's own default when the field is unset is 5, and that is what this reports rather than 0 —
// an unset field is not "keep nothing", and showing 0 would invite an operator to "fix" a number
// that was never wrong.
const DefaultTaskHistoryLimit int64 = 5

// TaskHistoryLimit reads the cluster's setting. Cluster-wide, so it must be asked of a manager.
func (e *Node) TaskHistoryLimit(ctx context.Context) (int64, error) {
	sw, err := e.Client.SwarmInspect(ctx)
	if err != nil {
		return 0, fmt.Errorf("dockerx: reading the swarm spec: %w", err)
	}
	if l := sw.Spec.Orchestration.TaskHistoryRetentionLimit; l != nil {
		return *l, nil
	}
	return DefaultTaskHistoryLimit, nil
}

// SetTaskHistoryLimit changes how many finished tasks Swarm keeps.
//
// Read-modify-write, and it has to be: Docker's swarm update replaces the WHOLE spec, so sending a
// freshly built one with only this field set would silently wipe the raft configuration, the
// dispatcher heartbeat and the CA config with it. We inspect, change the one field, and send it
// back at the version we read — which is also what makes a concurrent edit fail loudly (Docker
// refuses a stale version) rather than clobber.
//
// The rotation flags stay false. Rotating a join token is a different act with different
// consequences, and it is not something a retention change should do on the way past.
func (e *Node) SetTaskHistoryLimit(ctx context.Context, limit int64) error {
	sw, err := e.Client.SwarmInspect(ctx)
	if err != nil {
		return fmt.Errorf("dockerx: reading the swarm spec: %w", err)
	}

	spec := sw.Spec
	spec.Orchestration.TaskHistoryRetentionLimit = &limit

	if err := e.Client.SwarmUpdate(ctx, sw.Version, spec, swarm.UpdateFlags{}); err != nil {
		return fmt.Errorf("dockerx: updating the swarm spec: %w", err)
	}
	return nil
}

// SwarmLeave takes THIS daemon out of the swarm.
//
// force is required for the last manager, because leaving with it dissolves the cluster: the raft
// store goes, and with it every service, secret and config definition. The services' containers keep
// running until something stops them, which makes the damage quiet as well as total.
func (e *Node) SwarmLeave(ctx context.Context, force bool) error {
	return e.Client.SwarmLeave(ctx, force)
}
