package monitor

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Mnshahawy/daffa/internal/dockerx"
)

// The collector computes CPU from raw counter deltas rather than letting the daemon do it, and
// these are the edge cases that buys us. Each one below is a real failure that produces a
// plausible-looking wrong number, which is the worst kind: a monitoring system that is
// obviously broken gets fixed, and one that is quietly wrong gets trusted.

const interval = 30 * time.Second

// newTestCollector seeds the CPU-allowance cache so the happy path never reaches for a Docker
// client. Everything under test here is arithmetic.
func newTestCollector(cores float64) *Collector {
	c := NewCollector(nil, nil, slog.New(slog.DiscardHandler))
	c.cores["c1"] = cores
	return c
}

// A container using one core of a ten-core host, sampled thirty seconds apart. One core of ten
// is 10% of the machine — and with no CPU limit, "its allowance" IS the machine, so 10%.
func TestCPUIsTheMeanOverTheInterval(t *testing.T) {
	c := newTestCollector(10) // no limit: allowance == the host's ten cores
	at := time.Now().UTC()

	// The first sighting establishes a baseline and yields nothing.
	if _, _, ok := c.cpu(context.Background(), nil, raw(0, 0), at, interval); ok {
		t.Fatal("the first sighting of a container produced a CPU sample — there is nothing " +
			"to difference it against, so any number it gave would be invented")
	}

	// Thirty seconds later: the container burned 30s of CPU time (one core, solid); the host
	// burned 30s × 10 cores.
	pct, cores, ok := c.cpu(context.Background(), nil,
		raw(30*1e9, 300*1e9), at.Add(interval), interval)
	if !ok {
		t.Fatal("a clean second sample produced nothing")
	}
	if cores != 10 {
		t.Errorf("allowance = %v cores; want 10", cores)
	}
	if pct < 9.9 || pct > 10.1 {
		t.Errorf("CPU = %.2f%%; want 10%% (one core of ten, for the whole interval)", pct)
	}
}

// The number that makes a rule mean what it says.
//
// A container limited to half a core, pegged completely solid, on a ten-core host: docker stats
// would call this 5% (of the machine) and no "CPU above 70%" rule would ever fire, no matter
// how thoroughly the thing is on fire. As a percentage of what it is ALLOWED, it is 100%.
func TestALimitedContainerAtItsLimitReadsOneHundred(t *testing.T) {
	c := newTestCollector(0.5) // --cpus=0.5
	at := time.Now().UTC()

	c.cpu(context.Background(), nil, raw(0, 0), at, interval)

	// It burned half a core for thirty seconds: 15s of CPU time. The host burned 300s.
	pct, _, ok := c.cpu(context.Background(), nil,
		raw(15*1e9, 300*1e9), at.Add(interval), interval)
	if !ok {
		t.Fatal("no sample")
	}
	if pct < 99 {
		t.Errorf("a container pegged at its 0.5-core limit reads %.1f%%; want ~100%%. "+
			"If this says 5, the percentage is of the HOST rather than of the container's "+
			"allowance, and no threshold anyone would write will ever fire.", pct)
	}
}

// `docker restart` keeps the container's id and resets its cgroup counters. The baseline we are
// differencing against therefore belongs to a life the container no longer remembers.
//
// Unguarded this produces a large negative CPU — or, when both deltas go negative and the signs
// cancel, a wildly positive one, which is worse: it would page somebody at 3am about a container
// that is doing nothing.
func TestARestartDoesNotInventCPU(t *testing.T) {
	c := newTestCollector(10)
	at := time.Now().UTC()

	c.cpu(context.Background(), nil, raw(500*1e9, 5000*1e9), at, interval)

	// The container restarts: its counter is back to nearly zero. The HOST's counter keeps
	// climbing, as it always does.
	_, _, ok := c.cpu(context.Background(), nil,
		raw(1*1e9, 5300*1e9), at.Add(interval), interval)
	if ok {
		t.Error("a restarted container (id preserved, cgroup counters reset) produced a CPU " +
			"sample. The delta is negative and the reading is fiction.")
	}

	// And the round after that is fine again — the restart cost one sample, not the container.
	pct, _, ok := c.cpu(context.Background(), nil,
		raw(31*1e9, 5600*1e9), at.Add(2*interval), interval)
	if !ok {
		t.Fatal("the collector did not recover after a restart — it must re-baseline, not give up")
	}
	if pct < 9 || pct > 11 {
		t.Errorf("after re-baselining, CPU = %.1f%%; want ~10%%", pct)
	}
}

// The host was unreachable for five minutes. Its counters kept climbing the whole time, and the
// container's did too — so a delta across that gap is a five-minute average, and writing it
// down against a single timestamp claims it is a thirty-second one. It would flatten a spike
// that happened inside the gap and overstate a quiet patch.
func TestAGapReBaselinesRatherThanAveragingAcrossIt(t *testing.T) {
	c := newTestCollector(10)
	at := time.Now().UTC()

	c.cpu(context.Background(), nil, raw(0, 0), at, interval)

	_, _, ok := c.cpu(context.Background(), nil,
		raw(300*1e9, 3000*1e9), at.Add(5*time.Minute), interval)
	if ok {
		t.Error("a five-minute gap was differenced as though it were one interval — the " +
			"number is a five-minute mean wearing a thirty-second timestamp")
	}

	// The next round, a proper interval later, works.
	if _, _, ok := c.cpu(context.Background(), nil,
		raw(330*1e9, 3300*1e9), at.Add(5*time.Minute+interval), interval); !ok {
		t.Error("the collector did not recover after a gap")
	}
}

// A container cannot use more than it is allowed, but the daemon's counters round, and the
// kernel will let a container overshoot slightly inside one CFS period. A chart with a 103%
// peak on it makes a person distrust every other number on the page.
func TestCPUIsClamped(t *testing.T) {
	c := newTestCollector(1)
	at := time.Now().UTC()

	c.cpu(context.Background(), nil, raw(0, 0), at, interval)

	// Slightly more CPU time than one core could possibly have provided.
	pct, _, ok := c.cpu(context.Background(), nil,
		raw(31*1e9, 300*1e9), at.Add(interval), interval)
	if !ok {
		t.Fatal("no sample")
	}
	if pct > 100 {
		t.Errorf("CPU = %.2f%%, which is more than all of it", pct)
	}
}

// Docker's raw memory usage includes the page cache, so a container that has ever read a file
// looks like it is hoarding memory. Without this subtraction, every memory alert fires on every
// container on the first night and is muted by the second.
func TestPageCacheIsNotMemoryInUse(t *testing.T) {
	for _, key := range []string{"inactive_file", "total_inactive_file", "cache"} {
		got := dockerx.MemoryInUseForTest(1000, map[string]uint64{key: 600})
		if got != 400 {
			t.Errorf("with %s=600 of 1000 used, memory in use = %d; want 400", key, got)
		}
	}

	// No cache figure at all (an old kernel, an odd runtime): use what we were given rather
	// than guess.
	if got := dockerx.MemoryInUseForTest(1000, nil); got != 1000 {
		t.Errorf("with no cache stat, memory in use = %d; want the raw 1000", got)
	}

	// And a cache larger than usage — which the kernel does report, transiently — must not
	// underflow a uint64 into sixteen exabytes of memory use.
	if got := dockerx.MemoryInUseForTest(100, map[string]uint64{"inactive_file": 500}); got != 0 {
		t.Errorf("cache > usage produced %d; an unsigned underflow here reads as 16 EB", got)
	}
}

func raw(cpuTotal, systemTotal uint64) dockerx.Raw {
	return dockerx.Raw{
		ContainerID: "c1",
		CPUTotal:    cpuTotal,
		SystemTotal: systemTotal,
		OnlineCPUs:  10,
		MemBytes:    1000,
		MemLimit:    2000,
	}
}
