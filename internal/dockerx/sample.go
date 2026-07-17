package dockerx

// The sampler behind resource monitoring. See docs/monitoring.md.
//
// This is deliberately NOT Snapshot (stats.go), even though both ask a daemon how a container
// is doing, and the difference is worth understanding before anyone merges the two.
//
// Snapshot serves the live stats panel: someone is looking at the screen, they want a number
// now, and Docker's stream=false gives it — by blocking for about a second while the daemon
// collects two frames and computes the CPU delta between them for us.
//
// A sampler cannot use that. Two reasons, both measured against a real daemon:
//
//   - stream=false costs ~1025ms per container; one-shot costs ~17ms. Sixty times, on every
//     container, on every host, every thirty seconds, forever.
//
//   - Far worse, it would LIE. stream=false measures CPU across a ~1 second window. Sample
//     that every 30 seconds and you have observed 3% of the elapsed time — so a container
//     that spikes for five seconds a minute reads as idle, and "CPU above 70% for ten
//     minutes" is a rule about almost nothing.
//
// So we take Docker's raw cumulative counters and difference them ourselves, across the full
// sampling interval. The CPU figure that comes out is the true MEAN over those thirty seconds,
// which is the only thing an alert can honestly be written against. The price is that we own
// the edge cases the daemon was handling for us, and they are in monitor/collector.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/docker/docker/api/types/container"
)

// Raw is one reading of a container's cumulative counters. Meaningless alone; it becomes a
// CPU percentage only when differenced against the previous one.
type Raw struct {
	ContainerID string

	// CPUTotal is the container's cumulative CPU time, in nanoseconds.
	CPUTotal uint64
	// SystemTotal is the HOST's cumulative CPU time across every core, in nanoseconds. The
	// ratio of the two deltas is the container's share of the machine.
	SystemTotal uint64
	// OnlineCPUs is how many cores that host total is spread across.
	OnlineCPUs float64

	// Memory is a gauge, not a counter — it needs no previous reading and is true as it
	// stands. Docker's raw usage includes the page cache, which makes an idle container look
	// like it is hoarding memory; this has already had it subtracted, as `docker stats` does.
	MemBytes uint64
	MemLimit uint64
}

// SampleAll reads every named container's counters, concurrently.
//
// Containers that vanish between the list and the read are skipped, not failed: on a busy host
// something is always exiting, and a collector that gave up on the whole round because one
// container finished would collect nothing at all on exactly the hosts worth watching.
func (e *Node) SampleAll(ctx context.Context, ids []string) ([]Raw, error) {
	const maxConcurrent = 16
	sem := make(chan struct{}, maxConcurrent)

	var (
		mu  sync.Mutex
		out = make([]Raw, 0, len(ids))
		wg  sync.WaitGroup
	)

	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			r, err := e.sampleOne(ctx, id)
			if err != nil {
				return
			}
			mu.Lock()
			out = append(out, *r)
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Node) sampleOne(ctx context.Context, id string) (*Raw, error) {
	// ContainerStatsOneShot: returns immediately with a single frame. PreCPUStats comes back
	// zeroed — which is precisely why the live-stats path cannot use it, and precisely why we
	// can: we keep our own previous frame, from thirty seconds ago rather than one second ago.
	resp, err := e.Client.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("dockerx: sampling %s: %w", id, err)
	}
	defer resp.Body.Close()

	var s container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("dockerx: decoding sample for %s: %w", id, err)
	}

	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		cpus = 1
	}

	return &Raw{
		ContainerID: id,
		CPUTotal:    s.CPUStats.CPUUsage.TotalUsage,
		SystemTotal: s.CPUStats.SystemUsage,
		OnlineCPUs:  cpus,
		MemBytes:    memoryInUse(&s),
		MemLimit:    s.MemoryStats.Limit,
	}, nil
}

// memoryInUse is Docker's raw usage minus the page cache.
//
// Without the subtraction every container that has ever read a file looks like it is at 90% of
// its limit, because the kernel keeps the pages around and charges them to the cgroup. It is
// what `docker stats` shows, and it is the number a memory alert has to be written against or
// every alert fires on every container on the first night.
//
// cgroup v2 calls the reclaimable part `inactive_file`; v1 called it `total_inactive_file` and
// older kernels only had `cache`. Try each.
func memoryInUse(s *container.StatsResponse) uint64 {
	usage := s.MemoryStats.Usage
	for _, key := range []string{"inactive_file", "total_inactive_file", "cache"} {
		if cache, ok := s.MemoryStats.Stats[key]; ok {
			if cache < usage {
				return usage - cache
			}
			return 0
		}
	}
	return usage
}

// MemoryInUseForTest exposes memoryInUse to the collector's tests without exporting the
// StatsResponse plumbing around it. The page-cache subtraction is load-bearing enough to
// deserve a test, and an unsigned underflow in it reads as sixteen exabytes of memory use.
func MemoryInUseForTest(usage uint64, stats map[string]uint64) uint64 {
	var s container.StatsResponse
	s.MemoryStats.Usage = usage
	s.MemoryStats.Stats = stats
	return memoryInUse(&s)
}

// CPULimit is how many cores a container is ALLOWED, which is what its CPU percentage should
// be a percentage of.
//
// This is not docker stats' number. `docker stats` reports a percentage of one core, so a
// container on an eight-core box legitimately reads 800%, and a container limited to half a
// core and pegged completely solid reads 6% of the host — a figure that will never trip a
// "CPU above 70%" rule no matter how thoroughly the thing is on fire.
//
// A percentage of the ALLOWANCE says 100%, which is true, and it makes `cpu > 70` and
// `memory > 70` mean the same kind of thing, which is what makes a rule readable.
//
// Falls back to the host's cores when there is no limit, which is the honest reading of "it may
// use the whole machine".
func (e *Node) CPULimit(ctx context.Context, id string, onlineCPUs float64) float64 {
	info, err := e.Client.ContainerInspect(ctx, id)
	if err != nil || info.HostConfig == nil {
		return onlineCPUs
	}

	// --cpus / compose `cpus:` — the modern spelling.
	if n := info.HostConfig.NanoCPUs; n > 0 {
		return float64(n) / 1e9
	}
	// --cpu-quota / --cpu-period — the older one, and what some compose files still emit.
	if q, p := info.HostConfig.CPUQuota, info.HostConfig.CPUPeriod; q > 0 && p > 0 {
		return float64(q) / float64(p)
	}
	return onlineCPUs
}
