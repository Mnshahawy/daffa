package dockerx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
)

// ── images ──────────────────────────────────────────────────────────────────────

type Image struct {
	ID       string   `json:"id"`
	Tags     []string `json:"tags"`
	Size     int64    `json:"size"`
	Created  int64    `json:"created"`
	Dangling bool     `json:"dangling"`
	InUse    bool     `json:"in_use"`

	// The machine this is on. Node-local resources fan out across a swarm; see dockerx.Container.
	Node   string `json:"node,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

// ListImages reports which images are actually in use, because "can I delete this?" is
// the only question anyone opens an image list to answer.
func (e *Node) ListImages(ctx context.Context) ([]Image, error) {
	imgs, err := e.Client.ImageList(ctx, image.ListOptions{All: false})
	if err != nil {
		return nil, fmt.Errorf("dockerx: listing images on %s: %w", e.Name, err)
	}

	inUse, err := e.imagesInUse(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Image, 0, len(imgs))
	for _, i := range imgs {
		item := Image{
			ID:      i.ID,
			Tags:    i.RepoTags,
			Size:    i.Size,
			Created: i.Created,
			// Docker reports an untagged image as <none>:<none>, or with no tags at all.
			Dangling: len(i.RepoTags) == 0 || (len(i.RepoTags) == 1 && i.RepoTags[0] == "<none>:<none>"),
			InUse:    inUse[i.ID],
		}
		if item.Dangling {
			item.Tags = nil
		}
		out = append(out, item)
	}

	sort.Slice(out, func(a, b int) bool { return out[a].Size > out[b].Size }) // biggest first: that is what you came to reclaim
	return out, nil
}

// imagesInUse maps image IDs that a container (running OR stopped) depends on. A
// stopped container still pins its image, and deleting it out from under them is how
// you discover that at the worst possible moment.
func (e *Node) imagesInUse(ctx context.Context) (map[string]bool, error) {
	containers, err := e.ListContainers(ctx, true)
	if err != nil {
		return nil, err
	}

	inUse := map[string]bool{}
	for _, c := range containers {
		info, err := e.Client.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue // raced with a removal; it is not pinning anything any more
		}
		inUse[info.Image] = true
	}
	return inUse, nil
}

func (e *Node) RemoveImage(ctx context.Context, id string, force bool) error {
	_, err := e.Client.ImageRemove(ctx, id, image.RemoveOptions{Force: force, PruneChildren: true})
	if err != nil {
		return fmt.Errorf("dockerx: removing image %s: %w", id, err)
	}
	return nil
}

// ── volumes ─────────────────────────────────────────────────────────────────────

type Volume struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Created    string   `json:"created"`
	Size       int64    `json:"size"` // -1 when the driver cannot report it
	UsedBy     []string `json:"used_by"`
	Mountpoint string   `json:"mountpoint"`
	// System marks a volume this deployment depends on (DAFFA_SYSTEM_VOLUMES) — refused
	// for removal, like a system network. Unlike networks there are no built-in system
	// volumes, so the daemon never sets this; the API layer does, from config.
	System bool `json:"system"`

	// The machine this is on. Node-local resources fan out across a swarm; see dockerx.Container.
	Node   string `json:"node,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

// ListVolumes annotates each volume with the containers that mount it — the
// archaeology you otherwise do by hand before daring to delete one.
func (e *Node) ListVolumes(ctx context.Context) ([]Volume, error) {
	resp, err := e.Client.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("dockerx: listing volumes on %s: %w", e.Name, err)
	}

	usedBy, err := e.volumeUsers(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Volume, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		item := Volume{
			Name:       v.Name,
			Driver:     v.Driver,
			Created:    v.CreatedAt,
			Size:       -1,
			Mountpoint: v.Mountpoint,
			UsedBy:     usedBy[v.Name],
		}
		if v.UsageData != nil {
			item.Size = v.UsageData.Size
		}
		out = append(out, item)
	}

	// Orphans first — they are the actionable ones.
	sort.Slice(out, func(a, b int) bool {
		if (len(out[a].UsedBy) == 0) != (len(out[b].UsedBy) == 0) {
			return len(out[a].UsedBy) == 0
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

func (e *Node) volumeUsers(ctx context.Context) (map[string][]string, error) {
	containers, err := e.ListContainers(ctx, true)
	if err != nil {
		return nil, err
	}

	users := map[string][]string{}
	for _, c := range containers {
		info, err := e.Client.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}
		for _, m := range info.Mounts {
			if m.Type == "volume" && m.Name != "" {
				users[m.Name] = append(users[m.Name], c.Name)
			}
		}
	}
	return users, nil
}

func (e *Node) RemoveVolume(ctx context.Context, name string, force bool) error {
	if err := e.Client.VolumeRemove(ctx, name, force); err != nil {
		return fmt.Errorf("dockerx: removing volume %s: %w", name, err)
	}
	return nil
}

// ── networks ────────────────────────────────────────────────────────────────────

type Network struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Scope      string   `json:"scope"`
	Internal   bool     `json:"internal"`
	Containers []string `json:"containers"`
	System     bool     `json:"system"` // Docker's own — see IsSystemNetwork

	// The machine this is on. Node-local resources fan out across a swarm; see dockerx.Container.
	Node   string `json:"node,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

// IsSystemNetwork reports whether a network is one of Docker's own — bridge, host and
// none exist on every daemon, containers land on them by default or by explicit request,
// and a daemon without them is broken. They are not Daffa's (or anyone's) to change.
func IsSystemNetwork(name string) bool {
	return name == "bridge" || name == "host" || name == "none"
}

// ErrSystemNetwork is the refusal to touch one. A sentinel because the API owes the
// caller a 400 that names the reason, not the daemon's raw error dressed as a 502.
var ErrSystemNetwork = errors.New("dockerx: bridge, host and none are Docker's own networks — they cannot be changed or removed")

func (e *Node) ListNetworks(ctx context.Context) ([]Network, error) {
	nets, err := e.Client.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("dockerx: listing networks on %s: %w", e.Name, err)
	}

	out := make([]Network, 0, len(nets))
	for _, n := range nets {
		item := Network{
			ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope,
			Internal: n.Internal,
			System:   IsSystemNetwork(n.Name),
		}

		// The list endpoint omits attached containers; only inspect has them.
		if full, err := e.Client.NetworkInspect(ctx, n.ID, network.InspectOptions{}); err == nil {
			for _, c := range full.Containers {
				item.Containers = append(item.Containers, c.Name)
			}
			sort.Strings(item.Containers)
		}
		out = append(out, item)
	}

	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// NetworkName resolves a network id (or name) to its canonical name, so the API layer can
// check it against the deployment's protected list before asking the daemon to remove it.
func (e *Node) NetworkName(ctx context.Context, id string) (string, error) {
	full, err := e.Client.NetworkInspect(ctx, id, network.InspectOptions{})
	if err != nil {
		return "", err
	}
	return full.Name, nil
}

func (e *Node) RemoveNetwork(ctx context.Context, id string) error {
	// Resolved by inspect, not trusted from the path: the guard must hold whether the
	// caller said "bridge" or its id. The daemon would refuse too — but with an error
	// written for dockerd's logs, not for the person who clicked.
	if full, err := e.Client.NetworkInspect(ctx, id, network.InspectOptions{}); err == nil &&
		IsSystemNetwork(full.Name) {
		return ErrSystemNetwork
	}
	if err := e.Client.NetworkRemove(ctx, id); err != nil {
		return fmt.Errorf("dockerx: removing network %s: %w", id, err)
	}
	return nil
}

// ── disk usage & prune ──────────────────────────────────────────────────────────

type DiskUsage struct {
	Images      Usage `json:"images"`
	Containers  Usage `json:"containers"`
	Volumes     Usage `json:"volumes"`
	BuildCache  Usage `json:"build_cache"`
	TotalSize   int64 `json:"total_size"`
	Reclaimable int64 `json:"reclaimable"`
}

type Usage struct {
	Count       int   `json:"count"`
	Size        int64 `json:"size"`
	Reclaimable int64 `json:"reclaimable"`
}

func (e *Node) DiskUsage(ctx context.Context) (*DiskUsage, error) {
	du, err := e.Client.DiskUsage(ctx, DiskUsageOptions())
	if err != nil {
		return nil, fmt.Errorf("dockerx: reading disk usage on %s: %w", e.Name, err)
	}

	out := &DiskUsage{}

	inUse, _ := e.imagesInUse(ctx)
	for _, i := range du.Images {
		out.Images.Count++
		out.Images.Size += i.Size
		if !inUse[i.ID] {
			out.Images.Reclaimable += i.Size
		}
	}

	for _, c := range du.Containers {
		out.Containers.Count++
		out.Containers.Size += c.SizeRw
		if c.State != "running" {
			out.Containers.Reclaimable += c.SizeRw
		}
	}

	for _, v := range du.Volumes {
		out.Volumes.Count++
		if v.UsageData == nil {
			continue
		}
		out.Volumes.Size += v.UsageData.Size
		if v.UsageData.RefCount == 0 {
			out.Volumes.Reclaimable += v.UsageData.Size
		}
	}

	for _, b := range du.BuildCache {
		if b.Shared {
			continue // counted once already under whatever owns it
		}
		out.BuildCache.Count++
		out.BuildCache.Size += b.Size
		if !b.InUse {
			out.BuildCache.Reclaimable += b.Size
		}
	}

	out.TotalSize = out.Images.Size + out.Containers.Size + out.Volumes.Size + out.BuildCache.Size
	out.Reclaimable = out.Images.Reclaimable + out.Containers.Reclaimable +
		out.Volumes.Reclaimable + out.BuildCache.Reclaimable
	return out, nil
}

// PruneTarget names what a prune touches. Volumes are separate from the rest and never
// part of an "everything" sweep: a pruned volume is deleted DATA, whereas a pruned
// image or container is a rebuildable artifact. Anyone who wants that must ask for it
// by name.
type PruneTarget string

const (
	PruneImages     PruneTarget = "images"     // dangling only
	PruneContainers PruneTarget = "containers" // stopped
	PruneNetworks   PruneTarget = "networks"   // unused
	PruneVolumes    PruneTarget = "volumes"    // ANONYMOUS unused only — see below
	PruneBuildCache PruneTarget = "build-cache"
)

func ValidPruneTarget(t PruneTarget) bool {
	switch t {
	case PruneImages, PruneContainers, PruneNetworks, PruneVolumes, PruneBuildCache:
		return true
	}
	return false
}

type PruneResult struct {
	Target  PruneTarget `json:"target"`
	Deleted int         `json:"deleted"`
	Freed   uint64      `json:"freed"`
	Items   []string    `json:"items,omitempty"`
}

func (e *Node) Prune(ctx context.Context, target PruneTarget) (*PruneResult, error) {
	res := &PruneResult{Target: target}
	args := filters.NewArgs()

	switch target {
	case PruneImages:
		// dangling=true is the default, but say it out loud: the alternative prunes
		// every image not currently used by a container, which on a deploy host means
		// the next rollback has to pull everything again.
		args.Add("dangling", "true")
		r, err := e.Client.ImagesPrune(ctx, args)
		if err != nil {
			return nil, fmt.Errorf("dockerx: pruning images: %w", err)
		}
		res.Freed = r.SpaceReclaimed
		for _, d := range r.ImagesDeleted {
			res.Deleted++
			if d.Deleted != "" {
				res.Items = append(res.Items, shortID(d.Deleted))
			}
		}

	case PruneContainers:
		r, err := e.Client.ContainersPrune(ctx, args)
		if err != nil {
			return nil, fmt.Errorf("dockerx: pruning containers: %w", err)
		}
		res.Freed = r.SpaceReclaimed
		res.Deleted = len(r.ContainersDeleted)
		for _, id := range r.ContainersDeleted {
			res.Items = append(res.Items, shortID(id))
		}

	case PruneNetworks:
		r, err := e.Client.NetworksPrune(ctx, args)
		if err != nil {
			return nil, fmt.Errorf("dockerx: pruning networks: %w", err)
		}
		res.Deleted = len(r.NetworksDeleted)
		res.Items = r.NetworksDeleted

	case PruneVolumes:
		// NOT `all`: without this filter Docker prunes every unused volume, including
		// NAMED ones — the database volume of a stack that happens to be stopped. The
		// default here deletes only anonymous volumes, which are by definition
		// throwaway. Named volumes are removed one at a time, on purpose, by a human.
		r, err := e.Client.VolumesPrune(ctx, args)
		if err != nil {
			return nil, fmt.Errorf("dockerx: pruning volumes: %w", err)
		}
		res.Freed = r.SpaceReclaimed
		res.Deleted = len(r.VolumesDeleted)
		res.Items = r.VolumesDeleted

	case PruneBuildCache:
		r, err := e.Client.BuildCachePrune(ctx, BuildCachePruneOptions())
		if err != nil {
			return nil, fmt.Errorf("dockerx: pruning build cache: %w", err)
		}
		res.Freed = r.SpaceReclaimed
		res.Deleted = len(r.CachesDeleted)

	default:
		return nil, fmt.Errorf("dockerx: unknown prune target %q", target)
	}

	return res, nil
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
