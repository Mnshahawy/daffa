package dockerx

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"
)

// This file is every CLUSTER-WIDE question Daffa asks, and it is the other half of the split the
// package doc describes. These methods still hang off a Node — because a question is always asked
// of a daemon — but the daemon they must be asked of is a MANAGER. Env.Control() is the only thing
// that hands one out, and it is the only thing that should.
//
// Nothing here needed a new transport. Every one of these calls was already on the ordinary Docker
// client the pool has always built, local socket or agent tunnel alike, which is why Swarm costs
// no new plumbing and nothing new in the agent.

// SwarmInfo is what a daemon says about its own place in a Swarm. It is read from Info(), which is
// a call the liveness ping already makes, so reconciling costs nothing extra.
type SwarmInfo struct {
	// InSwarm is false for a daemon that has never run `swarm init` or `swarm join`. That daemon
	// is standalone, and there is nothing further to say about it.
	InSwarm bool
	// NodeID is this daemon's identity WITHIN the swarm, and the join key that later turns a
	// task's NodeID into the client that can exec into it.
	NodeID string
	// ClusterID identifies the swarm itself. Two daemons reporting the same one are, definitionally,
	// the same cluster — which is how environments are assembled rather than asserted. Docker
	// populates it ONLY on managers, so it is empty for a worker: an active worker is in the swarm
	// (InSwarm true) but cannot name it, and the reconcile resolves such a node via a manager.
	ClusterID string
	// Manager reports Swarm.ControlAvailable: not "is this labelled a manager" but "will this
	// socket answer a question about the cluster". It is the only bit that decides a control node.
	Manager bool
	Leader  bool

	Managers int
	Nodes    int
}

// Swarm asks a daemon what it is. A standalone daemon answers honestly and cheaply.
func (e *Node) Swarm(ctx context.Context) (*SwarmInfo, error) {
	info, err := e.Client.Info(ctx)
	if err != nil {
		return nil, err
	}

	s := info.Swarm

	// LocalNodeState is inactive | pending | active | error | locked. Only "active" means this
	// daemon is actually part of a working swarm; the rest are a daemon that is either not in one
	// or cannot currently be said to be in one, and both are "not a swarm" as far as anything
	// Daffa does with the answer. This is the ONLY thing that decides swarm membership — a nil
	// Cluster does not, see below.
	if s.LocalNodeState != swarm.LocalNodeStateActive {
		return &SwarmInfo{}, nil
	}

	out := &SwarmInfo{
		InSwarm:  true,
		NodeID:   s.NodeID,
		Manager:  s.ControlAvailable,
		Managers: s.Managers,
		Nodes:    s.Nodes,
	}

	// Swarm.Cluster is a POINTER, and Docker populates it — and thus the ClusterID — ONLY on a
	// manager. A worker in an active swarm reports it nil. So a nil Cluster is NOT "standalone"
	// once the state is active: it is simply a worker that cannot name its own cluster. Reading
	// Cluster.ID unconditionally panics on that worker (and on the standalone default), so guard
	// it; leaving ClusterID empty is the signal the reconcile uses to resolve the node via a
	// manager's node list instead of by cluster id.
	if s.Cluster != nil {
		out.ClusterID = s.Cluster.ID
	}

	// Leadership is not in Info(); it is on the node's own record, and only a manager can read
	// that record. A worker simply does not know who the leader is, which is fine — it is never
	// going to be asked to be one.
	if out.Manager && out.NodeID != "" {
		if n, _, err := e.Client.NodeInspectWithRaw(ctx, out.NodeID); err == nil {
			out.Leader = n.ManagerStatus != nil && n.ManagerStatus.Leader
		}
	}
	return out, nil
}

// Service is one swarm service, flattened to what an operator reads.
type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Mode is replicated | global. A global service has no replica count — it runs one task per
	// node — and showing it as "3/3 replicas" would be a fluent lie. See Desired.
	Mode string `json:"mode"`
	// Image as the service SPEC holds it, which after a deploy is digest-pinned:
	// `nginx:1.25@sha256:…`. Tag is that same image with the digest cut off, which is the only
	// form worth comparing a compose file against.
	Image string `json:"image"`
	Tag   string `json:"tag"`

	Desired int `json:"desired"`
	Running int `json:"running"`

	Ports []string `json:"ports,omitempty"`

	// Stack is the swarm stack this service belongs to, if any. Swarm stamps it; nothing else does.
	Stack string `json:"stack,omitempty"`

	// UpdateState is how the LAST update ended, and it is the reason this struct exists rather
	// than the raw spec: swarm records `rollback_completed` when it gave up on a deploy and put
	// the old spec back. A deploy that reported success and then silently rolled back is the exact
	// gap between "the command worked" and "the thing is running", and neither Portainer nor
	// Dokploy surfaces it.
	UpdateState   string    `json:"update_state,omitempty"` // updating|paused|completed|rollback_started|rollback_completed|…
	UpdateMessage string    `json:"update_message,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Task is one attempt to run one replica of a service, on one node.
//
// THE TASK IS THE POINT. A service that says 0/3 tells you nothing; the task underneath it says
// `no suitable node (insufficient memory on 2 nodes)`, and that is the whole answer. It lives in
// Status.Err, and it is the single most useful string Swarm produces.
type Task struct {
	ID     string `json:"id"`
	Slot   int    `json:"slot"`
	NodeID string `json:"node_id"` // the SWARM node id; join it against ours to get a client

	// Desired is what swarm WANTS this task to be (running, shutdown, …); State is what it
	// actually is. The pair is the diagnosis: desired=running, state=pending means it cannot be
	// placed, and Err says why.
	Desired string `json:"desired"`
	State   string `json:"state"`
	Err     string `json:"error,omitempty"`

	ContainerID string    `json:"container_id,omitempty"`
	Image       string    `json:"image,omitempty"`
	Since       time.Time `json:"since"`
}

// SwarmNode is swarm's view of one machine.
type SwarmNode struct {
	ID           string `json:"id"`
	Hostname     string `json:"hostname"`
	Role         string `json:"role"`         // manager | worker
	Availability string `json:"availability"` // active | pause | drain
	State        string `json:"state"`        // ready | down | disconnected | …
	Leader       bool   `json:"leader"`

	Version string `json:"version"`
	CPUs    int64  `json:"cpus"`
	Memory  int64  `json:"memory"`
	Addr    string `json:"addr,omitempty"`
}

const (
	labelStackNamespace = "com.docker.stack.namespace"
	labelServiceName    = "com.docker.swarm.service.name"
)

// ListServices returns every service in the cluster, with its task counts already resolved.
//
// The counts come from the tasks rather than from the spec, because the spec says what was ASKED
// for and the tasks say what happened — and the difference between those two numbers is the only
// reason anyone opens this page.
func (e *Node) ListServices(ctx context.Context) ([]Service, error) {
	svcs, err := e.Client.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, err
	}

	tasks, err := e.Client.TaskList(ctx, types.TaskListOptions{})
	if err != nil {
		return nil, err
	}

	running := map[string]int{}
	for _, t := range tasks {
		if t.DesiredState == swarm.TaskStateRunning && t.Status.State == swarm.TaskStateRunning {
			running[t.ServiceID]++
		}
	}

	nodeCount := 0
	if nodes, err := e.Client.NodeList(ctx, types.NodeListOptions{}); err == nil {
		nodeCount = len(nodes)
	}

	out := make([]Service, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, serviceOf(s, running[s.ID], nodeCount))
	}
	return out, nil
}

func (e *Node) InspectService(ctx context.Context, id string) (*Service, error) {
	s, _, err := e.Client.ServiceInspectWithRaw(ctx, id, types.ServiceInspectOptions{})
	if err != nil {
		return nil, err
	}

	f := filters.NewArgs()
	f.Add("service", s.ID)
	tasks, err := e.Client.TaskList(ctx, types.TaskListOptions{Filters: f})
	if err != nil {
		return nil, err
	}

	running := 0
	for _, t := range tasks {
		if t.DesiredState == swarm.TaskStateRunning && t.Status.State == swarm.TaskStateRunning {
			running++
		}
	}

	nodeCount := 0
	if nodes, err := e.Client.NodeList(ctx, types.NodeListOptions{}); err == nil {
		nodeCount = len(nodes)
	}

	svc := serviceOf(s, running, nodeCount)
	return &svc, nil
}

func serviceOf(s swarm.Service, running, nodeCount int) Service {
	image := s.Spec.TaskTemplate.ContainerSpec.Image

	out := Service{
		ID:      s.ID,
		Name:    s.Spec.Name,
		Image:   image,
		Tag:     UntagDigest(image),
		Running: running,
		Stack:   s.Spec.Labels[labelStackNamespace],
	}

	switch {
	case s.Spec.Mode.Replicated != nil:
		out.Mode = "replicated"
		if r := s.Spec.Mode.Replicated.Replicas; r != nil {
			out.Desired = int(*r)
		}
	case s.Spec.Mode.Global != nil:
		// A global service runs one task per node. It has no replica count, and inventing one
		// would be the "N/N replicas" lie: the honest number is the node count.
		out.Mode = "global"
		out.Desired = nodeCount
	default:
		out.Mode = "replicated"
	}

	for _, p := range s.Endpoint.Ports {
		if p.PublishedPort != 0 {
			out.Ports = append(out.Ports, portString(p))
		}
	}

	if s.UpdateStatus != nil {
		out.UpdateState = string(s.UpdateStatus.State)
		out.UpdateMessage = s.UpdateStatus.Message
	}
	out.UpdatedAt = s.UpdatedAt
	return out
}

func portString(p swarm.PortConfig) string {
	s := fmt.Sprintf("%d:%d", p.PublishedPort, p.TargetPort)
	if p.Protocol != "tcp" {
		s += "/" + string(p.Protocol)
	}
	return s
}

// UntagDigest strips the digest swarm pins onto an image at deploy time.
//
// `docker stack deploy` defaults to --resolve-image=always, so it resolves every tag against the
// registry and writes the result back into the spec as `nginx:1.25@sha256:…`. That pinning is a
// FEATURE — it is what guarantees every node runs the same bytes — but it means a naive compare of
// the declared image against the running one reports every healthy service as drifted, forever.
// Compare on what is before the '@'.
func UntagDigest(image string) string {
	if i := strings.Index(image, "@"); i > 0 {
		return image[:i]
	}
	return image
}

// ListTasks returns a service's tasks, newest first.
func (e *Node) ListTasks(ctx context.Context, serviceID string) ([]Task, error) {
	f := filters.NewArgs()
	f.Add("service", serviceID)

	tasks, err := e.Client.TaskList(ctx, types.TaskListOptions{Filters: f})
	if err != nil {
		return nil, err
	}

	out := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		task := Task{
			ID:      t.ID,
			Slot:    t.Slot,
			NodeID:  t.NodeID,
			Desired: string(t.DesiredState),
			State:   string(t.Status.State),
			// The string that is the whole reason this page exists.
			Err:   t.Status.Err,
			Image: UntagDigest(t.Spec.ContainerSpec.Image),
			Since: t.Status.Timestamp,
		}
		if t.Status.ContainerStatus != nil {
			task.ContainerID = t.Status.ContainerStatus.ContainerID
		}
		out = append(out, task)
	}

	sortTasks(out)
	return out, nil
}

// sortTasks puts the newest first. The task somebody is looking for is the one that just failed.
func sortTasks(t []Task) {
	sort.Slice(t, func(i, j int) bool {
		if t[i].Since.Equal(t[j].Since) {
			return t[i].Slot < t[j].Slot
		}
		return t[i].Since.After(t[j].Since)
	})
}

// ListSwarmNodes is what the SWARM says its machines are — which is not the same question as what
// Daffa can reach. The API layer joins the two, and that join is the whole value of the page.
func (e *Node) ListSwarmNodes(ctx context.Context) ([]SwarmNode, error) {
	nodes, err := e.Client.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, err
	}

	out := make([]SwarmNode, 0, len(nodes))
	for _, n := range nodes {
		sn := SwarmNode{
			ID:           n.ID,
			Hostname:     n.Description.Hostname,
			Role:         string(n.Spec.Role),
			Availability: string(n.Spec.Availability),
			State:        string(n.Status.State),
			Leader:       n.ManagerStatus != nil && n.ManagerStatus.Leader,
			Version:      n.Description.Engine.EngineVersion,
			Addr:         n.Status.Addr,
		}
		if r := n.Description.Resources; r.NanoCPUs > 0 || r.MemoryBytes > 0 {
			sn.CPUs = r.NanoCPUs / 1e9
			sn.Memory = r.MemoryBytes
		}
		out = append(out, sn)
	}
	return out, nil
}

// ServiceLogs follows a service's logs across the WHOLE cluster.
//
// This is the one cluster-wide stream Docker proxies for us: the manager collects from every node
// running a task, so it works with no agent on the workers at all. Exec and stats are not proxied
// and never will be — see Env.NodeBySwarmID, which is how those get routed instead.
//
// Two things beyond a container log. First, each line is ATTRIBUTED: a merged stream of many
// replicas is unreadable if you cannot tell which one spoke, so Details is enabled and the task/node
// the daemon tags each line with is resolved to "service.slot" on a named machine. Second, an
// unreachable node is a WARNING, not the end of the stream — warn carries that notice so the logs of
// every reachable task keep flowing rather than being truncated by one dead worker.
func (e *Node) ServiceLogs(ctx context.Context, id, tail string, follow bool, emit func(LogLine) error, warn func(string) error) error {
	// Resolve the hex task/node ids the daemon tags lines with, once, into names a person reads.
	taskName, nodeName := e.logAttribution(ctx, id)

	rc, err := e.Client.ServiceLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
		Follow:     follow,
		Timestamps: false,
		Details:    true, // prepend each line with its task/node — the whole point of attribution
	})
	if err != nil {
		return fmt.Errorf("dockerx: streaming logs for service %s: %w", id, err)
	}
	defer rc.Close()

	// Close the reader when the client goes away, or a followed stream keeps the goroutine (and
	// the daemon connection) alive forever. Same hazard as container logs, same answer.
	go func() {
		<-ctx.Done()
		_ = rc.Close()
	}()

	// Every line arrives as "k=v,k=v <message>" (Details); attributeLine strips that prefix and
	// swaps the ids for names before the line goes out.
	attribute := func(l LogLine) error {
		return emit(attributeLine(l, taskName, nodeName))
	}
	// Service logs are ALWAYS multiplexed. There is no TTY case to fall back to: a service has
	// many tasks on many machines, so there is no single terminal for them to share.
	stdout, stderr := newLineWriter("stdout", attribute), newLineWriter("stderr", attribute)

	// Why a LOOP around StdCopy. When a node is unavailable the daemon does NOT fail the stream — it
	// writes a "some logs could not be retrieved" notice on its system-error channel and keeps
	// sending the reachable tasks' logs. But StdCopy stops dead at that frame and returns it as an
	// error, so taken at face value one unreachable worker would truncate the logs of every healthy
	// one — exactly the followed-stream failure this replaces. So the system-error is caught,
	// surfaced once as a warning, and reading resumes on the next frame. The daemon closes the
	// stream when it is genuinely finished, and StdCopy then returns a clean nil, which ends the loop.
	warned := map[string]bool{}
	for {
		_, err := stdcopy.StdCopy(stdout, stderr, rc)
		if err == nil {
			break // the daemon closed the stream: done
		}
		if ctx.Err() != nil || isClosed(err) {
			break // the client hung up; not an error
		}
		if note, ok := daemonStreamNote(err); ok {
			if warn != nil && !warned[note] {
				warned[note] = true
				if werr := warn(note); werr != nil {
					return werr
				}
			}
			continue // resume: the reachable tasks are still streaming
		}
		return fmt.Errorf("dockerx: demultiplexing logs for service %s: %w", id, err)
	}
	if err := stdout.flush(); err != nil {
		return err
	}
	return stderr.flush()
}

// logAttribution builds the id→name maps a service log needs: swarm task id → "service.slot", and
// swarm node id → hostname. Best-effort — a line from a task older than the daemon's task-history
// limit simply falls back to a short id, which is still better than nothing.
func (e *Node) logAttribution(ctx context.Context, id string) (task, node map[string]string) {
	task, node = map[string]string{}, map[string]string{}

	name := id
	if svc, _, err := e.Client.ServiceInspectWithRaw(ctx, id, types.ServiceInspectOptions{}); err == nil {
		name = svc.Spec.Name
	}

	f := filters.NewArgs()
	f.Add("service", id)
	if tasks, err := e.Client.TaskList(ctx, types.TaskListOptions{Filters: f}); err == nil {
		for _, t := range tasks {
			if t.Slot > 0 {
				task[t.ID] = fmt.Sprintf("%s.%d", name, t.Slot) // replicated: the replica index
			} else {
				task[t.ID] = name // global: one per node — the Node field is what tells them apart
			}
		}
	}
	if nodes, err := e.Client.NodeList(ctx, types.NodeListOptions{}); err == nil {
		for _, n := range nodes {
			node[n.ID] = n.Description.Hostname
		}
	}
	return task, node
}

// attributeLine lifts the task/node ids off a Details-tagged service log line and swaps them for
// names. The daemon prepends "k=v,k=v " to every line when Details is set — url-escaped, sorted by
// key (api/server/httputils.WriteLogStream), so the values never contain a space or comma and the
// FIRST space is the boundary before the message. A line that is not that shape is passed through.
func attributeLine(l LogLine, taskName, nodeName map[string]string) LogLine {
	prefix, msg, ok := strings.Cut(l.Text, " ")
	if !ok || !strings.Contains(prefix, "com.docker.swarm.") {
		return l
	}

	var taskID, nodeID string
	for _, kv := range strings.Split(prefix, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		val, err := url.QueryUnescape(v)
		if err != nil {
			val = v
		}
		switch k {
		case "com.docker.swarm.task.id":
			taskID = val
		case "com.docker.swarm.node.id":
			nodeID = val
		}
	}
	if taskID == "" && nodeID == "" {
		return l // not the prefix we expected — leave the text untouched
	}

	l.Text = msg
	if taskID != "" {
		if name := taskName[taskID]; name != "" {
			l.Task = name
		} else {
			l.Task = shortHex(taskID)
		}
	}
	if nodeID != "" {
		if host := nodeName[nodeID]; host != "" {
			l.Node = host
		} else {
			l.Node = shortHex(nodeID)
		}
	}
	return l
}

// daemonStreamNote recognises the frame StdCopy surfaces when the daemon reports a problem MID
// stream — for service logs, "some logs could not be retrieved: node X is not available". StdCopy
// wraps a system-error frame with a fixed prefix; anything else is a real demux/read failure.
func daemonStreamNote(err error) (string, bool) {
	const prefix = "error from daemon in stream: "
	if s := err.Error(); strings.HasPrefix(s, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(s, prefix)), true
	}
	return "", false
}

// shortHex is the fallback label for an id no lookup resolved — the leading chars Docker itself
// shows, enough to tell two apart without filling a line.
func shortHex(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
