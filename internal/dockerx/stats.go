package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/docker/docker/api/types/container"
)

// Stats is one sample for one container, already reduced to the numbers a human reads.
// Docker's raw stats payload is enormous and mostly cumulative counters; turning those
// into rates is the server's job, not the browser's.
type Stats struct {
	ID       string  `json:"id"`
	CPU      float64 `json:"cpu"`       // percent of all host CPUs
	Memory   uint64  `json:"memory"`    // bytes in use
	MemLimit uint64  `json:"mem_limit"` // bytes available
	MemPct   float64 `json:"mem_pct"`
	NetRx    uint64  `json:"net_rx"`
	NetTx    uint64  `json:"net_tx"`
	BlockR   uint64  `json:"block_read"`
	BlockW   uint64  `json:"block_write"`
}

// Snapshot returns one sample per container, fanned out concurrently.
//
// This is what the LIST view uses. The alternative — holding an open stats stream per
// row — is what makes a container dashboard peg a CPU on a busy host: each stream is a
// goroutine, a daemon connection, and a 1Hz JSON payload, multiplied by every row on
// screen whether or not anyone is looking at it. A snapshot costs one request each,
// only while the tab is open, and only for the rows actually rendered.
func (e *Node) Snapshot(ctx context.Context, ids []string) ([]Stats, error) {
	// Bound the fan-out: a host with 200 containers should not open 200 simultaneous
	// connections to its own daemon.
	const maxConcurrent = 16
	sem := make(chan struct{}, maxConcurrent)

	var (
		mu  sync.Mutex
		out = make([]Stats, 0, len(ids))
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

			st, err := e.oneShot(ctx, id)
			if err != nil {
				// A container that stopped between the list and the sample is normal,
				// not an error worth failing the whole request over.
				return
			}
			mu.Lock()
			out = append(out, *st)
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Node) oneShot(ctx context.Context, id string) (*Stats, error) {
	// Note this is ContainerStats(stream=false), NOT ContainerStatsOneShot. CPU percent
	// is a delta between two samples, and the true one-shot endpoint returns
	// PreCPUStats zeroed — so it can tell you memory but every container reads 0% CPU.
	// stream=false asks the daemon's collector for a single frame that still carries
	// the previous sample, which is the only cheap way to get a real number.
	resp, err := e.Client.ContainerStats(ctx, id, false)
	if err != nil {
		return nil, fmt.Errorf("dockerx: stats for %s: %w", id, err)
	}
	defer resp.Body.Close()

	var raw container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("dockerx: decoding stats for %s: %w", id, err)
	}
	return reduce(id, &raw), nil
}

// StreamStats follows one container's stats. This is the DETAIL view: exactly one
// stream, for the one container someone is actually looking at.
func (e *Node) StreamStats(ctx context.Context, id string, emit func(Stats) error) error {
	resp, err := e.Client.ContainerStats(ctx, id, true)
	if err != nil {
		return fmt.Errorf("dockerx: streaming stats for %s: %w", id, err)
	}
	defer resp.Body.Close()

	// Cancel on return so the watchdog is released when the stream ends by itself, not only when
	// the request context does — otherwise it parks on <-ctx.Done() for the tab's whole lifetime.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = resp.Body.Close()
	}()

	dec := json.NewDecoder(resp.Body)
	for {
		var raw container.StatsResponse
		if err := dec.Decode(&raw); err != nil {
			if ctx.Err() != nil || isClosed(err) {
				return nil
			}
			return fmt.Errorf("dockerx: decoding stats stream for %s: %w", id, err)
		}
		if err := emit(*reduce(id, &raw)); err != nil {
			return err
		}
	}
}

// reduce turns Docker's cumulative counters into the rates and percentages a person
// can act on.
func reduce(id string, raw *container.StatsResponse) *Stats {
	s := &Stats{ID: id}

	// CPU percent is the container's CPU-time delta over the host's CPU-time delta,
	// scaled by the number of CPUs — i.e. 100% means "one whole core", and a container
	// on an 8-core box can legitimately report 800%.
	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage) - float64(raw.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(raw.CPUStats.SystemUsage) - float64(raw.PreCPUStats.SystemUsage)
	if sysDelta > 0 && cpuDelta > 0 {
		cpus := float64(raw.CPUStats.OnlineCPUs)
		if cpus == 0 {
			cpus = float64(len(raw.CPUStats.CPUUsage.PercpuUsage))
		}
		if cpus == 0 {
			cpus = 1
		}
		s.CPU = (cpuDelta / sysDelta) * cpus * 100.0
	}

	// Docker's memory usage includes the page cache, which makes an idle container
	// look like it is hoarding memory. Subtracting it is what `docker stats` does.
	usage := raw.MemoryStats.Usage
	if cache, ok := raw.MemoryStats.Stats["inactive_file"]; ok && cache < usage {
		usage -= cache
	} else if cache, ok := raw.MemoryStats.Stats["cache"]; ok && cache < usage {
		usage -= cache
	}
	s.Memory = usage
	s.MemLimit = raw.MemoryStats.Limit
	if s.MemLimit > 0 {
		s.MemPct = float64(s.Memory) / float64(s.MemLimit) * 100.0
	}

	for _, n := range raw.Networks {
		s.NetRx += n.RxBytes
		s.NetTx += n.TxBytes
	}

	for _, b := range raw.BlkioStats.IoServiceBytesRecursive {
		switch b.Op {
		case "read", "Read":
			s.BlockR += b.Value
		case "write", "Write":
			s.BlockW += b.Value
		}
	}
	return s
}
