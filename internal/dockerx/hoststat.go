package dockerx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
)

// HostStat is a machine's raw resource counters, straight from /proc. CPU is cumulative jiffies —
// a delta between two reads gives utilization — and memory is a point-in-time total/available.
type HostStat struct {
	CPUTotal   uint64 // sum of the fields on the aggregate `cpu` line of /proc/stat
	CPUIdle    uint64 // idle + iowait
	MemTotalKB uint64
	MemAvailKB uint64
}

// HostDiskStat is the machine's ROOT filesystem — total capacity and how much of it is used, in
// bytes. This is the whole box's disk (the one Docker's data-root lives on in an ordinary install),
// NOT Docker's own layer footprint: the Cluster page shows the two side by side so "607 MB is
// reclaimable" reads against "of 30 GB, 5 GB used" instead of floating with no denominator.
type HostDiskStat struct {
	Total int64 `json:"total"` // bytes
	Used  int64 `json:"used"`  // bytes
	Free  int64 `json:"free"`  // bytes available to unprivileged writers (df "Available")
}

// HostStats reads the machine's /proc/stat and /proc/meminfo through a short-lived probe container.
//
// The Docker API exposes no live host CPU/memory, and Daffa keeps no persistent agent on a managed
// host (docs/clusters.md) — so it does what the deploy runner does: an ephemeral container over the
// SAME API, here bind-mounting the host /proc read-only and printing the two files. It therefore
// works identically for a local socket, an agent tunnel, or an SSH node, which is the whole point.
// The reader is in a container but /host/proc is the machine's /proc, so the numbers are the host's.
func (e *Node) HostStats(ctx context.Context, probeImage string) (*HostStat, error) {
	cfg := &container.Config{
		Image: probeImage,
		Cmd:   []string{"cat", "/host/proc/stat", "/host/proc/meminfo"},
	}
	host := &container.HostConfig{
		Mounts: []mount.Mount{{Type: mount.TypeBind, Source: "/proc", Target: "/host/proc", ReadOnly: true}},
	}
	out, err := e.runProbe(ctx, "host-stats", cfg, host)
	if err != nil {
		return nil, err
	}
	return parseHostStat(out)
}

// HostDisk reports the machine's root-filesystem capacity and usage through the same probe pattern.
//
// It bind-mounts the host root read-only and runs `df` against it — `df` reports the filesystem
// backing a path, so `df /host` is the host's root fs even though /host is a bind mount. The mount
// is NON-recursive on purpose: df only needs the root mountpoint's numbers, and pulling every
// submount of / (proc, sys, other containers' volumes) into the probe would be a far bigger hammer
// than the question deserves.
func (e *Node) HostDisk(ctx context.Context, probeImage string) (*HostDiskStat, error) {
	cfg := &container.Config{
		Image: probeImage,
		Cmd:   []string{"df", "-P", "-k", "/host"}, // -P: one line per fs, no wrapping; -k: 1024-byte blocks
	}
	host := &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:        mount.TypeBind,
			Source:      "/",
			Target:      "/host",
			ReadOnly:    true,
			BindOptions: &mount.BindOptions{NonRecursive: true},
		}},
	}
	out, err := e.runProbe(ctx, "host-disk", cfg, host)
	if err != nil {
		return nil, err
	}
	return parseHostDisk(out)
}

// runProbe runs one ephemeral probe container to completion and returns its combined output. It is
// the shared body of HostStats/HostDisk: pull-if-missing, create, start, wait, read, demux, remove.
// name is only used to make the error strings say which probe failed.
func (e *Node) runProbe(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig) (string, error) {
	if err := e.ensureImage(ctx, cfg.Image); err != nil {
		return "", err
	}

	created, err := e.Client.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("dockerx: creating %s probe: %w", name, err)
	}
	defer e.Client.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})

	if err := e.Client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("dockerx: starting %s probe: %w", name, err)
	}

	statusCh, errCh := e.Client.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return "", fmt.Errorf("dockerx: waiting for %s probe: %w", name, err)
		}
	case <-statusCh:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	rc, err := e.Client.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", fmt.Errorf("dockerx: reading %s probe: %w", name, err)
	}
	defer rc.Close()

	var buf bytes.Buffer
	// No TTY, so the log stream is multiplexed and must be demuxed.
	if _, err := stdcopy.StdCopy(&buf, &buf, io.LimitReader(rc, 1<<20)); err != nil {
		return "", fmt.Errorf("dockerx: demuxing %s probe: %w", name, err)
	}
	return buf.String(), nil
}

// ensureImage pulls the probe image if the host does not have it. A host that has never deployed
// anything will not have docker:cli yet; the first host-stats round pulls it, once.
func (e *Node) ensureImage(ctx context.Context, img string) error {
	if _, _, err := e.Client.ImageInspectWithRaw(ctx, img); err == nil {
		return nil
	}
	rc, err := e.Client.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("dockerx: pulling probe image %s: %w", img, err)
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc) // the pull is complete only when the body is drained
	return err
}

// parseHostStat reads the aggregate `cpu` line of /proc/stat and MemTotal/MemAvailable of
// /proc/meminfo. The cpu fields are: user nice system idle iowait irq softirq steal guest guest_nice;
// idle time is idle+iowait.
func parseHostStat(out string) (*HostStat, error) {
	var h HostStat
	var sawCPU, sawMemTotal, sawMemAvail bool
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "cpu" && !sawCPU:
			for i, v := range f[1:] {
				n, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					continue
				}
				h.CPUTotal += n
				if i == 3 || i == 4 { // idle, iowait
					h.CPUIdle += n
				}
			}
			sawCPU = true
		case f[0] == "MemTotal:" && len(f) >= 2:
			h.MemTotalKB, _ = strconv.ParseUint(f[1], 10, 64)
			sawMemTotal = true
		case f[0] == "MemAvailable:" && len(f) >= 2:
			h.MemAvailKB, _ = strconv.ParseUint(f[1], 10, 64)
			sawMemAvail = true
		}
	}
	if !sawCPU || !sawMemTotal || !sawMemAvail {
		return nil, fmt.Errorf("dockerx: host-stats probe output incomplete (%d bytes)", len(out))
	}
	return &h, nil
}

// parseHostDisk reads `df -P -k /host`. POSIX output is a header line then exactly one data line for
// the filesystem containing the path: Filesystem, 1024-blocks, Used, Available, Capacity%, Mounted.
// KB are scaled to bytes to match Docker's DiskUsage, which the page renders with the same helper.
func parseHostDisk(out string) (*HostDiskStat, error) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		total, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue // the header row (and any warning line) — the numeric column is what we want
		}
		used, _ := strconv.ParseInt(f[2], 10, 64)
		free, _ := strconv.ParseInt(f[3], 10, 64)
		return &HostDiskStat{Total: total * 1024, Used: used * 1024, Free: free * 1024}, nil
	}
	return nil, fmt.Errorf("dockerx: host-disk probe output had no filesystem row (%d bytes)", len(out))
}
