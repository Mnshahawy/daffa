package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The collector, against a REAL Docker daemon.
//
// Set DAFFA_TEST_DOCKER_HOST to a daemon that may be loaded up and torn down — a dind, never
// your own — and prepare it with three containers:
//
//	docker run -d --name capped   --cpus=0.5 busybox sh -c 'while :; do :; done'
//	docker run -d --name uncapped            busybox sh -c 'while :; do :; done'
//	docker run -d --name idle                busybox sleep 3600
//
// Everything above this file is arithmetic that can be tested with invented numbers. This is
// the one thing that cannot: whether the numbers we get from a live daemon, differenced across
// a live interval, actually describe what the containers are doing.
func TestCollectorAgainstARealDaemon(t *testing.T) {
	host := os.Getenv("DAFFA_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("DAFFA_TEST_DOCKER_HOST not set — the live collector is NOT covered by this run")
	}
	ctx := context.Background()

	s := openStore(t)
	env := &store.Environment{Name: "dind"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	node := &store.Node{EnvID: env.ID, Name: "dind", Kind: "local", DockerHost: host}
	if err := s.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}

	pool := dockerx.NewPool()
	if err := pool.Register(env, node); err != nil {
		t.Fatalf("connecting to %s: %v", host, err)
	}
	t.Cleanup(pool.Close)

	c := NewCollector(s, pool, slog.New(slog.DiscardHandler))

	// Two rounds, five seconds apart. The first only establishes the baseline — there is
	// nothing to difference a first sighting against, which is exactly what the collector
	// says and exactly why it writes nothing.
	const interval = 5 * time.Second
	c.Collect(ctx, interval)

	if n := countRows(t, s); n != 0 {
		t.Fatalf("the first round wrote %d samples; a first sighting has no CPU delta to "+
			"report and must write nothing rather than write a zero", n)
	}

	time.Sleep(interval)
	c.Collect(ctx, interval)

	got := latest(t, s, env.ID)
	if len(got) < 3 {
		t.Fatalf("sampled %d containers; want at least 3 (capped, uncapped, idle). Is the "+
			"daemon prepared as the comment above describes?", len(got))
	}

	for name, m := range got {
		t.Logf("%-10s cpu %6.1f%% of %.2f cores   mem %s of %s (%.2f%%)",
			name, m.CPUPct, m.CPUCores, mib(m.MemBytes), mib(m.MemLimit), m.MemPct)
	}

	// ── the number this whole design turns on ────────────────────────────────────
	//
	// `capped` is limited to half a core and is burning all of it. Docker's own stats calls
	// that 50% — because docker stats is a percentage of ONE CORE — and as a share of this
	// ten-core host it is 5%. Neither number would ever trip a "CPU above 70%" rule, no matter
	// how completely the container is on fire.
	//
	// As a percentage of what it is ALLOWED, it is at 100%. That is the true statement, it is
	// the one a person means when they write a threshold, and it is what makes `cpu > 70` and
	// `memory > 70` mean the same kind of thing.
	capped, ok := got["capped"]
	if !ok {
		t.Fatal("the capped container was not sampled")
	}
	if capped.CPUCores < 0.4 || capped.CPUCores > 0.6 {
		t.Errorf("the capped container's allowance reads %.2f cores; want 0.5 — "+
			"HostConfig.NanoCPUs is not being read", capped.CPUCores)
	}
	if capped.CPUPct < 85 {
		t.Errorf("a container pegged at its 0.5-core limit reads %.1f%%.\n"+
			"If it is around 5, the percentage is of the HOST rather than of the container's "+
			"allowance — and no threshold anybody would write will ever fire on it.\n"+
			"If it is around 50, it is docker stats' percent-of-one-core, which has the same "+
			"problem in a subtler way.", capped.CPUPct)
	}

	// `uncapped` runs the identical loop with no limit. One core of ten, so ~10% — and it must
	// NOT read 100%, or the allowance is being ignored and every busy container looks maxed.
	uncapped, ok := got["uncapped"]
	if !ok {
		t.Fatal("the uncapped container was not sampled")
	}
	if uncapped.CPUPct > 40 {
		t.Errorf("an unlimited container using one core of ten reads %.1f%%; want ~10%%. "+
			"Its allowance is the whole machine.", uncapped.CPUPct)
	}
	if uncapped.CPUPct < 2 {
		t.Errorf("a container burning a whole core reads %.1f%% — it is not being measured at all",
			uncapped.CPUPct)
	}

	// And the idle one reads idle. Without this, "everything is at 100%" would pass the two
	// assertions above and still be completely broken.
	if idle, ok := got["idle"]; !ok {
		t.Error("the idle container was not sampled")
	} else if idle.CPUPct > 5 {
		t.Errorf("a sleeping container reads %.1f%% CPU", idle.CPUPct)
	}

	// Memory: real numbers, page cache subtracted, and a limit to be a percentage of.
	for name, m := range got {
		if m.MemBytes <= 0 {
			t.Errorf("%s reports %d bytes of memory", name, m.MemBytes)
		}
		if m.MemLimit <= 0 {
			t.Errorf("%s has no memory limit — an unlimited container's limit is the host's "+
				"memory, which is what a percentage needs to be of", name)
		}
		if m.MemPct <= 0 || m.MemPct > 100 {
			t.Errorf("%s reports %.2f%% memory", name, m.MemPct)
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────────

func countRows(t *testing.T, s *store.Store) int {
	t.Helper()
	pts, err := s.Series(context.Background(), store.SeriesQuery{
		EnvID: "", From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute),
		MaxPoints: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(pts)
}

type liveSample struct {
	CPUPct   float64
	CPUCores float64
	MemBytes int64
	MemLimit int64
	MemPct   float64
}

// latest reads the most recent sample per container, straight out of the store.
func latest(t *testing.T, s *store.Store, env string) map[string]liveSample {
	t.Helper()

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Minute)
	src, err := s.MetricsSourceForTest(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.DB().Query(
		"SELECT container_name, cpu_pct, cpu_cores, mem_bytes, mem_limit, mem_pct FROM " +
			src + " WHERE env_id = '" + env + "'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	out := map[string]liveSample{}
	for rows.Next() {
		var (
			name string
			m    liveSample
		)
		if err := rows.Scan(&name, &m.CPUPct, &m.CPUCores, &m.MemBytes, &m.MemLimit, &m.MemPct); err != nil {
			t.Fatal(err)
		}
		out[name] = m
	}
	return out
}

func mib(b int64) string {
	return fmt.Sprintf("%.1f MiB", float64(b)/1024/1024)
}
