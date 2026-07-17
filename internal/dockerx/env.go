// Package dockerx owns Daffa's connection to Docker daemons.
//
// The whole multi-host design rests on one idea: every daemon — the local socket, an agent
// tunnel — is reached through the ordinary Docker Go client, differing only in how its
// connection is dialed. Feature code therefore never branches on "is this remote?", and remote
// machines get container ops, logs, exec and stats for free.
//
// # Node, and Env
//
// A NODE is one daemon. Every operation Docker performs against a single daemon — containers,
// images, volumes, exec, stats, prune — is a method on Node, and that is not a naming choice:
// it is how the node-local/cluster-wide split (docs/swarm.md §3) is made unrepresentable rather
// than merely documented. You cannot ask a Node a question it has no standing to answer, because
// the method is not there.
//
// An ENV is where things run: one node (standalone) or many (a swarm). It answers three
// questions, and which one a caller asks IS the split:
//
//	Control()          — the daemon that answers CLUSTER-WIDE questions. A manager.
//	Node(id) / One()   — one named daemon. Node-local work.
//	Nodes()            — all of them. Node-local work that fans out.
//
// Portainer expresses this same distinction as an HTTP header a caller can forget to set. We
// express it as a function you have to call, which is the version that cannot be forgotten.
package dockerx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"

	"github.com/docker/docker/client"

	"github.com/Mnshahawy/daffa/internal/store"
)

// Node is a live handle on ONE Docker daemon.
type Node struct {
	ID     string
	EnvID  string
	Name   string
	Kind   string // local | agent
	Client *client.Client

	// Swarm's view of this daemon, as of the last reconcile. The daemon is authoritative;
	// these are a cache. Empty on a standalone node.
	SwarmNodeID string
	SwarmRole   string // none | worker | manager
	Leader      bool
}

// Manager reports whether this daemon will answer cluster-wide questions — which is the only
// thing that actually matters when choosing a control node. Docker calls it ControlAvailable,
// and the question it answers is not "is this labelled a manager" but "will this socket answer
// me".
func (n *Node) Manager() bool { return n.SwarmRole == "manager" }

// Env is where things run: a standalone daemon, or a swarm of them.
type Env struct {
	ID      string
	Name    string
	SwarmID string // "" ⇒ standalone

	mu    sync.RWMutex
	nodes map[string]*Node // by node id
}

// IsSwarm mirrors store.Environment.IsSwarm: an environment is a swarm exactly when it has a
// swarm id. Nothing is stored twice.
func (e *Env) IsSwarm() bool { return e.SwarmID != "" }

// ErrNoControl is returned when nothing in this environment can answer a cluster-wide question.
//
// It is a REAL, reachable state, not a defensive check: enroll an agent on a swarm WORKER whose
// managers Daffa cannot reach, and you have a swarm environment with no control node. Node-local
// work on that node still works perfectly. Cluster-wide work has nobody to ask, and the honest
// thing is to say so — rather than return an empty service list, which is a lie shaped like an
// answer.
var ErrNoControl = fmt.Errorf("dockerx: no reachable swarm manager in this environment")

// Control is the daemon that answers cluster-wide questions: the leader if it is reachable,
// otherwise any manager. On a standalone environment it is the one node.
//
// This choice is made HERE and nowhere else, and it is never offered to a user: "which manager?"
// is a question with no meaningful answer, because every manager gives the same one.
func (e *Env) Control() (*Node, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if !e.IsSwarm() {
		for _, n := range e.nodes {
			return n, nil
		}
		return nil, fmt.Errorf("dockerx: environment %q has no nodes", e.Name)
	}

	var fallback *Node
	for _, n := range e.nodes {
		if !n.Manager() {
			continue
		}
		if n.Leader {
			return n, nil
		}
		if fallback == nil {
			fallback = n
		}
	}
	if fallback == nil {
		return nil, ErrNoControl
	}
	return fallback, nil
}

// One is the node to use when the caller did not name one — valid only when there is exactly one
// to choose from, which is the whole rule.
//
// The rule is ARITY, not kind: a standalone environment has one node, and so does a single-node
// swarm, which is the common production topology. A node becomes something you must NAME at
// exactly the moment it becomes something with more than one possible value. Keying this off the
// environment's kind instead would demand a redundant parameter from the deployment most people
// actually run.
func (e *Env) One() (*Node, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	switch len(e.nodes) {
	case 0:
		return nil, fmt.Errorf("dockerx: environment %q has no nodes", e.Name)
	case 1:
		for _, n := range e.nodes {
			return n, nil
		}
	}
	return nil, fmt.Errorf("dockerx: environment %q has %d nodes; name one", e.Name, len(e.nodes))
}

// Node reaches one named daemon.
func (e *Env) Node(id string) (*Node, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	n, ok := e.nodes[id]
	if !ok {
		return nil, fmt.Errorf("dockerx: no node %q in environment %q", id, e.Name)
	}
	return n, nil
}

// Nodes is the fan-out set for node-local reads, in a stable order so that two calls do not
// render the same list in two different sequences.
func (e *Env) Nodes() []*Node {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]*Node, 0, len(e.nodes))
	for _, n := range e.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NodeBySwarmID turns a task's NodeID into the client that can actually exec into it.
//
// This one method is the entire reason nodes carry a swarm_node_id, and it is why Daffa needs no
// agent-to-agent mesh: the server already holds a tunnel per node, so routing to the node a task
// runs on is a map lookup rather than a gossip protocol.
func (e *Env) NodeBySwarmID(swarmNodeID string) (*Node, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, n := range e.nodes {
		if n.SwarmNodeID != "" && n.SwarmNodeID == swarmNodeID {
			return n, true
		}
	}
	return nil, false
}

func (e *Env) put(n *Node) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nodes[n.ID] = n
}

func (e *Env) drop(nodeID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	if n, ok := e.nodes[nodeID]; ok {
		_ = n.Client.Close()
		delete(e.nodes, nodeID)
	}
	return len(e.nodes)
}

// Pool holds one Docker client per NODE, built lazily and reused. Clients are safe for concurrent
// use, and rebuilding one per request would waste a handshake on every poll of the container list.
type Pool struct {
	mu   sync.RWMutex
	envs map[string]*Env // by environment id
}

func NewPool() *Pool {
	return &Pool{envs: map[string]*Env{}}
}

// Dialer opens a connection to a daemon. For an agent this opens a stream inside the tunnel the
// agent dialed out on; the Docker client cannot tell the difference, which is the entire point.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Register wires a LOCAL node into the pool: a Docker client on a unix socket.
func (p *Pool) Register(env *store.Environment, node *store.Node) error {
	c, err := client.NewClientWithOpts(
		client.WithHost(node.DockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("dockerx: building client for %s: %w", node.Name, err)
	}
	p.attach(env, node, c)
	return nil
}

// RegisterAgent wires a REMOTE node into the pool, reached through dial.
//
// Note what is NOT here: no second API, no per-feature RPC, no "remote" flag threaded through the
// handlers. Containers, logs, exec, stats — and every Swarm call — go through the same
// client.Client as the local socket does.
func (p *Pool) RegisterAgent(env *store.Environment, node *store.Node, dial Dialer) error {
	c, err := client.NewClientWithOpts(
		// The host is a placeholder — the dialer decides where the bytes actually go.
		client.WithHost("http://daffa-agent"),
		client.WithHTTPClient(&http.Client{Transport: &http.Transport{
			DialContext: dial,
			// The tunnel already multiplexes; letting the HTTP transport pool connections on
			// top of it would keep streams open for no reason.
			DisableKeepAlives: true,
		}}),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("dockerx: building client for agent %s: %w", node.Name, err)
	}
	p.attach(env, node, c)
	return nil
}

func (p *Pool) attach(env *store.Environment, node *store.Node, c *client.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.envs[env.ID]
	if !ok {
		e = &Env{ID: env.ID, Name: env.Name, SwarmID: env.SwarmID, nodes: map[string]*Node{}}
		p.envs[env.ID] = e
	}
	e.SwarmID = env.SwarmID

	e.put(&Node{
		ID:          node.ID,
		EnvID:       env.ID,
		Name:        node.Name,
		Kind:        node.Kind,
		Client:      c,
		SwarmNodeID: node.SwarmNodeID,
		SwarmRole:   node.SwarmRole,
		Leader:      node.IsLeader,
	})
}

// Deregister drops a NODE (an agent went away). Its Docker client is closed so any in-flight
// request fails fast rather than hanging on a dead tunnel. When that empties the environment, the
// environment goes too — an environment Daffa can reach nothing through is not a thing to offer
// in a switcher.
func (p *Pool) Deregister(envID, nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.envs[envID]
	if !ok {
		return
	}
	if e.drop(nodeID) == 0 {
		delete(p.envs, envID)
	}
}

// Rename keeps the pool's cached label in step with a renamed environment, so log lines and error
// messages do not keep quoting a name the operator has already changed.
func (p *Pool) Rename(envID, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.envs[envID]; ok {
		e.Name = name
	}
}

// SetSwarm records what a node's daemon said about itself, and what swarm (if any) its environment
// turned out to be. The daemon is authoritative; this is the write that makes the cache agree.
// SetEnvSwarm records which swarm an environment turned out to be — or, with "", that it is
// standalone again because its daemon left one.
func (p *Pool) SetEnvSwarm(envID, swarmID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.envs[envID]; ok {
		e.SwarmID = swarmID
	}
}

func (p *Pool) SetSwarm(envID, nodeID, swarmID, swarmNodeID, role string, leader bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.envs[envID]
	if !ok {
		return
	}
	e.SwarmID = swarmID

	e.mu.Lock()
	defer e.mu.Unlock()
	if n, ok := e.nodes[nodeID]; ok {
		n.SwarmNodeID, n.SwarmRole, n.Leader = swarmNodeID, role, leader
	}
}

func (p *Pool) Get(envID string) (*Env, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	env, ok := p.envs[envID]
	if !ok {
		return nil, fmt.Errorf("dockerx: environment %q is not connected", envID)
	}
	return env, nil
}

func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, e := range p.envs {
		for _, n := range e.Nodes() {
			_ = n.Client.Close()
		}
	}
}
