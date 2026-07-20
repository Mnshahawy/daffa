// Package monitor samples what containers are using, keeps the samples for a while, and
// raises an alert when a rule is broken. See docs/monitoring.md.
package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// Compose stamps these on everything it creates; they are how a container knows which stack it
// belongs to. Daffa deploys with `-p <stack name>`, so the project label IS the stack's name.
// Swarm stamps its own set instead — a service task carries no compose labels at all — and its
// namespace label is, identically, the stack's name. A box can run both engines at once, so a
// container's stack is read from whichever pair is present (see stackOf/serviceOf). Without the
// swarm fallback, every swarm container samples with an empty stack: the target picker shows
// nothing for a swarm stack, and a stack-scoped monitor over one can never match a single row.
const (
	labelProject      = "com.docker.compose.project"
	labelService      = "com.docker.compose.service"
	labelSwarmStack   = "com.docker.stack.namespace"
	labelSwarmService = "com.docker.swarm.service.name"
)

// stackOf is the stack a container belongs to, compose or swarm. Compose wins when both are
// somehow present; otherwise the swarm namespace answers. "" means the container is in no stack.
func stackOf(labels map[string]string) string {
	if p := labels[labelProject]; p != "" {
		return p
	}
	return labels[labelSwarmStack]
}

// serviceOf is the container's service within its stack — compose's service name, or swarm's
// (which is `<stack>_<service>`, e.g. `teeeet1_app`). Display only; the stack is what monitors key on.
func serviceOf(labels map[string]string) string {
	if s := labels[labelService]; s != "" {
		return s
	}
	return labels[labelSwarmService]
}

type Collector struct {
	store *store.Store
	pool  *dockerx.Pool
	log   *slog.Logger

	// prev is the previous reading of each container's cumulative counters, keyed by container
	// id. CPU is a delta, and this is the thing it is a delta against.
	prev map[string]reading
	// cores caches each container's CPU allowance, keyed by container id. One inspect per
	// container, ever: the id changes on redeploy, so a container whose limit changed comes
	// back under a new key and the cache invalidates itself.
	cores map[string]float64

	// hostPrev is the previous /proc/stat reading of each NODE's machine, keyed by env/node id —
	// the baseline a host CPU% is a delta against, mirroring prev for containers.
	hostPrev map[string]hostReading
	// probeImage is the image the host-stats probe runs (docs/clusters.md): the same pinned
	// docker:cli the deploy runner uses, so it is already present after any deploy.
	probeImage string

	// eval is called with each round's timestamp once the samples are written. Split out so
	// the collector can be tested without an evaluator, and so the evaluator always runs
	// against the freshest sample rather than on a ticker of its own that races this one.
	eval func(ctx context.Context, at time.Time)
}

type reading struct {
	at          time.Time
	cpuTotal    uint64
	systemTotal uint64
}

type hostReading struct {
	at    time.Time
	total uint64
	idle  uint64
}

func NewCollector(st *store.Store, pool *dockerx.Pool, log *slog.Logger) *Collector {
	return &Collector{
		store:      st,
		pool:       pool,
		log:        log,
		prev:       map[string]reading{},
		cores:      map[string]float64{},
		hostPrev:   map[string]hostReading{},
		probeImage: stacks.RunnerImage,
	}
}

func (c *Collector) OnRound(f func(ctx context.Context, at time.Time)) { c.eval = f }

// Run samples on a ticker until the context is cancelled.
//
// The interval is re-read every round rather than captured once, so changing it in the UI takes
// effect on the next tick instead of at the next restart — and so does switching sampling off.
func (c *Collector) Run(ctx context.Context) {
	for {
		cfg, err := c.store.MonitorSettings(ctx)
		if err != nil {
			c.log.Error("monitor: reading settings", "err", err)
			cfg = &store.MonitorSettings{Enabled: true, IntervalSecs: 30, RetentionDays: 7}
		}

		if cfg.Enabled {
			c.Collect(ctx, cfg.Interval())
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.Interval()):
		}
	}
}

// Collect runs one round across every connected host.
func (c *Collector) Collect(ctx context.Context, interval time.Duration) {
	envs, err := c.store.ListEnvironments(ctx)
	if err != nil {
		c.log.Error("monitor: listing hosts", "err", err)
		return
	}

	// One timestamp for the whole round. Every sample in it therefore lands in one partition,
	// which is what lets the write be a single INSERT — and it means a chart's buckets line up
	// across the containers on a host instead of smearing across adjacent seconds.
	at := time.Now().UTC().Truncate(time.Second)

	// The live set is collected across the WHOLE round and forgotten once, at the end.
	//
	// It used to be forgotten per environment, which meant each environment's sweep deleted every
	// other environment's CPU baselines: the next round found no previous reading, cpu() returned
	// "no usable delta", and on any install with more than one environment **no CPU sample was
	// ever written** — silently, while memory kept working. A cluster made it worse still, because
	// each NODE would have swept the others.
	//
	// Baselines are keyed by container id, which is global. So is the sweep.
	alive := map[string]types.Container{}

	var samples []store.Sample
	for _, e := range envs {
		samples = append(samples, c.collectEnv(ctx, e.ID, at, interval, alive)...)
	}
	c.forget(alive)

	if len(samples) > 0 {
		if err := c.store.InsertSamples(ctx, samples); err != nil {
			c.log.Error("monitor: writing samples", "count", len(samples), "err", err)
			return
		}
	}

	if c.eval != nil {
		c.eval(ctx, at)
	}
}

// collectEnv samples every NODE in the environment. Containers are node-local — a manager cannot
// see the containers on another machine — so a cluster is sampled by asking each of its daemons,
// not by asking one of them a question it cannot answer. On a standalone environment that is a
// loop over one node, which is exactly what it used to be.
func (c *Collector) collectEnv(ctx context.Context, envID string, at time.Time, interval time.Duration, alive map[string]types.Container) []store.Sample {
	env, err := c.pool.Get(envID)
	if err != nil {
		// The environment is not connected. Say nothing: watchHosts already owns "this is down",
		// and a second voice saying it every thirty seconds is just noise in the log that
		// someone will eventually silence — along with the one that mattered.
		return nil
	}

	var out []store.Sample
	for _, node := range env.Nodes() {
		out = append(out, c.collectNode(ctx, envID, node, at, interval, alive)...)
	}
	return out
}

// nodeSampleTimeout bounds one node's whole contribution to a round: the container list, every
// stats call under it, and the CPU-limit inspect. A round samples nodes one after another, so
// without this a single wedged daemon — a half-open agent tunnel, a hung dockerd — would block on
// ContainerList until the SERVER shut down, and because that stall sits in front of every later
// node the evaluator never runs and monitoring goes silent fleet-wide, with no log line. The
// timeout turns "forever" into "this host costs at most one deadline, then the round moves on".
const nodeSampleTimeout = 15 * time.Second

func (c *Collector) collectNode(ctx context.Context, envID string, node *dockerx.Node, at time.Time, interval time.Duration, alive map[string]types.Container) []store.Sample {
	ctx, cancel := context.WithTimeout(ctx, nodeSampleTimeout)
	defer cancel()

	out := c.collectContainers(ctx, envID, node, at, interval, alive)

	// The machine itself — recorded even when the node runs no containers, which is the whole
	// difference from before: a bare host still gets CPU/memory history, and the Cluster page shows
	// the box's load rather than the sum of its containers.
	if hs := c.hostSample(ctx, envID, node, at, interval); hs != nil {
		out = append(out, *hs)
	}
	return out
}

// collectContainers samples every container on the node — the original per-container path.
func (c *Collector) collectContainers(ctx context.Context, envID string, node *dockerx.Node, at time.Time, interval time.Duration, alive map[string]types.Container) []store.Sample {
	list, err := node.Client.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		c.log.Debug("monitor: listing containers", "env", envID, "err", err)
		return nil
	}
	if len(list) == 0 {
		return nil
	}

	ids := make([]string, 0, len(list))
	meta := make(map[string]types.Container, len(list))
	for _, ct := range list {
		ids = append(ids, ct.ID)
		meta[ct.ID] = ct
		alive[ct.ID] = ct // the round's live set; swept once, by Collect
	}

	raws, err := node.SampleAll(ctx, ids)
	if err != nil {
		c.log.Debug("monitor: sampling", "env", envID, "err", err)
		return nil
	}

	out := make([]store.Sample, 0, len(raws))
	for _, r := range raws {
		ct, ok := meta[r.ContainerID]
		if !ok {
			continue
		}

		cpuPct, cores, ok := c.cpu(ctx, node, r, at, interval)
		if !ok {
			// No usable CPU delta this round (first sighting, a restart, a gap). Memory is a
			// gauge and is still true — but we drop the whole sample rather than write a row
			// with a zero CPU, because a zero is not "unknown", it is a lie that a sustained
			// alert would happily count as "not breaching" and reset its clock on.
			continue
		}

		s := store.Sample{
			TS:            at,
			EnvID:         envID,
			ContainerID:   shortID(r.ContainerID),
			ContainerName: containerName(ct),
			Stack:         stackOf(ct.Labels),
			Service:       serviceOf(ct.Labels),
			CPUPct:        cpuPct,
			CPUCores:      cores,
			MemBytes:      int64(r.MemBytes),
			MemLimit:      int64(r.MemLimit),
		}
		if r.MemLimit > 0 {
			s.MemPct = float64(r.MemBytes) / float64(r.MemLimit) * 100
		}
		out = append(out, s)
	}

	return out
}

// hostSample reads the machine's CPU% and memory through the host-stats probe and turns it into a
// sentinel Sample (container_name = store.HostSentinel). Memory is a gauge and is always recorded;
// CPU needs a delta against the previous round, so on the first sighting the row carries the memory
// and a zero CPU rather than being dropped — the machine memory is worth more than the one blip.
func (c *Collector) hostSample(ctx context.Context, envID string, node *dockerx.Node, at time.Time, interval time.Duration) *store.Sample {
	stat, err := node.HostStats(ctx, c.probeImage)
	if err != nil {
		c.log.Debug("monitor: host stats", "env", envID, "node", node.Name, "err", err)
		return nil
	}

	used := int64(stat.MemTotalKB-stat.MemAvailKB) * 1024
	total := int64(stat.MemTotalKB) * 1024
	// container_id carries the NODE id for a host row, so a multi-node Swarm's per-node machine
	// metrics can be read back one node at a time (Series filters on it), while the whole-cluster
	// query still sums every node's :host rows.
	s := &store.Sample{
		TS: at, EnvID: envID, ContainerID: node.ID, ContainerName: store.HostSentinel,
		MemBytes: used, MemLimit: total,
	}
	if total > 0 {
		s.MemPct = float64(used) / float64(total) * 100
	}
	if pct, ok := c.hostCPU(envID+"/"+node.ID, stat, at, interval); ok {
		s.CPUPct = pct
	}
	return s
}

// hostCPU turns two /proc/stat readings into a machine CPU% (0–100), keeping a per-node baseline
// exactly the way cpu() does for containers. Utilisation is one minus the idle share of the delta.
func (c *Collector) hostCPU(key string, stat *dockerx.HostStat, at time.Time, interval time.Duration) (float64, bool) {
	last, seen := c.hostPrev[key]
	c.hostPrev[key] = hostReading{at: at, total: stat.CPUTotal, idle: stat.CPUIdle}
	if !seen {
		return 0, false // first sighting: no delta yet
	}
	if !at.After(last.at) || at.Sub(last.at) > 3*interval {
		return 0, false // stale baseline: the host was unreachable, or sampling was toggled off and on
	}
	totalDelta := float64(stat.CPUTotal) - float64(last.total)
	idleDelta := float64(stat.CPUIdle) - float64(last.idle)
	if totalDelta <= 0 || idleDelta < 0 {
		return 0, false // counters went backwards — a reboot
	}
	pct := (1 - idleDelta/totalDelta) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// cpu differences this reading against the last one, and is where every edge case of owning
// the delta ourselves gets paid for. Each of these is a real failure, not a hypothetical.
func (c *Collector) cpu(ctx context.Context, node *dockerx.Node, r dockerx.Raw, at time.Time, interval time.Duration) (pct, cores float64, ok bool) {
	last, seen := c.prev[r.ContainerID]
	c.prev[r.ContainerID] = reading{at: at, cpuTotal: r.CPUTotal, systemTotal: r.SystemTotal}

	// First sighting: there is nothing to difference against. One missing sample, then it is
	// fine forever.
	if !seen {
		return 0, 0, false
	}

	// A stale baseline. The host was unreachable, or sampling was switched off and back on. A
	// delta across that gap is a true average of a five-minute window being written down as if
	// it were a thirty-second one — so it would understate a spike that happened inside it and
	// overstate a quiet patch. Re-baseline instead.
	if at.Sub(last.at) > 3*interval {
		return 0, 0, false
	}

	cpuDelta := float64(r.CPUTotal) - float64(last.cpuTotal)
	sysDelta := float64(r.SystemTotal) - float64(last.systemTotal)

	// A NEGATIVE delta. `docker restart` keeps the container's id but resets its cgroup
	// counters, and so does restarting the daemon — so the counter we are differencing against
	// belongs to a life the container no longer remembers. Unguarded, this produces a large
	// negative CPU, or (worse) a wildly positive one when both deltas go negative and the signs
	// cancel. Drop it; the next round has a fresh baseline.
	if cpuDelta < 0 || sysDelta <= 0 {
		return 0, 0, false
	}

	cores, cached := c.cores[r.ContainerID]
	if !cached {
		cores = node.CPULimit(ctx, r.ContainerID, r.OnlineCPUs)
		c.cores[r.ContainerID] = cores
	}
	if cores <= 0 {
		cores = r.OnlineCPUs
	}

	// The container's share of the whole machine, scaled up to a share of what it is ALLOWED.
	// A container limited to half a core, using all of it, on a ten-core host: it has 5% of the
	// machine, and that is 100% of its allowance. The second number is the one worth alerting
	// on. See dockerx.CPULimit.
	hostShare := cpuDelta / sysDelta * r.OnlineCPUs // 0..OnlineCPUs, in cores
	pct = hostShare / cores * 100

	// Clamp. Rounding in the daemon's counters, and a container briefly over its quota inside
	// one CFS period, can both put this a hair over 100 — and a chart with a 103% peak on it
	// makes a person distrust every other number on the page.
	if pct > 100 {
		pct = 100
	}
	return pct, cores, true
}

// forget drops per-container state for containers that are no longer on the host.
// forget drops the baselines of containers that no longer exist anywhere, or the maps grow
// forever on a host that redeploys ten times a day. It is called ONCE per round, with the live set
// of every node in every environment — see Collect.
func (c *Collector) forget(alive map[string]types.Container) {
	for id := range c.prev {
		if _, ok := alive[id]; !ok {
			delete(c.prev, id)
			delete(c.cores, id)
		}
	}
}

// containerName is the name without Docker's leading slash. Compose names it
// `<project>-<service>-<n>`, which is stable across a redeploy — unlike the id, which is why
// alerts key on this.
func containerName(ct types.Container) string {
	if len(ct.Names) == 0 {
		return shortID(ct.ID)
	}
	name := ct.Names[0]
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}
	return name
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
