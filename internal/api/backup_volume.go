package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/Mnshahawy/daffa/internal/backups"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
	"github.com/Mnshahawy/daffa/internal/volumes"
)

// The volume backup engine: where a database engine execs the database's own dump tool,
// the volume engine touches no user container at all — a read-only helper mount and the
// daemon's own tar stream, into the same gzip → age → S3 pipe. See docs/volumes.md.

func (s *Server) performVolumeBackup(ctx context.Context, job *store.BackupJob) (*backups.Result, error) {
	node, err := s.volumeNode(ctx, job.EnvID, job.Volume, false)
	if err != nil {
		return nil, err
	}
	_, dst, err := s.jobConfig(ctx, job)
	if err != nil {
		return nil, err
	}

	// Consistency, if the job asked for it: stop the named consumers for the duration of
	// the snapshot. The restart runs even when the snapshot fails — a failed backup must
	// not leave someone's service down.
	restart, err := stopForSnapshot(ctx, node, job.StopContainers)
	if err != nil {
		return nil, err
	}

	result, backupErr := func() (*backups.Result, error) {
		snap, err := volumes.Snapshot(ctx, node, job.Volume)
		if err != nil {
			return nil, err
		}
		defer snap.Close()
		return backups.RunVolume(ctx, snap, job.Volume, dst, time.Now())
	}()

	if err := restart(); err != nil {
		if backupErr != nil {
			return result, fmt.Errorf("%w; and afterwards: %v", backupErr, err)
		}
		// The backup is in the bucket, but a consumer is still down — that is the louder
		// of the two facts, and the one the notification must carry.
		return result, err
	}
	return result, backupErr
}

// volumeNode finds the ONE node holding the volume. A volume is node-local, and the same
// name on two nodes is two different data sets (the swarm volume trap) — backing up
// "whichever" would succeed against the wrong one, which is worse than refusing.
//
// forRestore relaxes exactly one case: the volume existing NOWHERE, on a single-node
// environment, resolves to that node — disaster recovery targets a box that no longer
// has the volume, and that is the one caller allowed to conjure it back.
func (s *Server) volumeNode(ctx context.Context, envID, volumeName string, forRestore bool) (*dockerx.Node, error) {
	env, err := s.pool.Get(envID)
	if err != nil {
		return nil, fmt.Errorf("the environment for this job is not connected")
	}

	var holders []*dockerx.Node
	for _, node := range env.Nodes() {
		if _, err := node.Client.VolumeInspect(ctx, volumeName); err == nil {
			holders = append(holders, node)
		}
	}
	switch len(holders) {
	case 1:
		return holders[0], nil
	case 0:
		if forRestore {
			if node, err := env.One(); err == nil {
				return node, nil
			}
			return nil, fmt.Errorf("volume %q exists on no node of this environment — on a multi-node environment, create it on the target node first so the restore has an unambiguous destination", volumeName)
		}
		return nil, fmt.Errorf("no volume named %q on any node Daffa can reach in this environment", volumeName)
	default:
		names := make([]string, 0, len(holders))
		for _, n := range holders {
			names = append(names, n.Name)
		}
		return nil, fmt.Errorf("a volume named %q exists on %d nodes (%s) — those are different data sets, and backing up whichever one answered would quietly be the wrong one. Remove the strays, or name the volumes per node",
			volumeName, len(holders), strings.Join(names, ", "))
	}
}

// stopForSnapshot stops the listed containers and returns the restart. A stop that fails
// aborts the backup: the job asked for consistency in writing, and snapshotting live
// anyway would deliver exactly what the operator opted out of.
func stopForSnapshot(ctx context.Context, node *dockerx.Node, targets string) (restart func() error, err error) {
	names := strings.Fields(targets)
	var stopped []string

	restart = func() error {
		var errs []string
		// Reverse order, the courtesy of an orderly shutdown undone.
		for i := len(stopped) - 1; i >= 0; i-- {
			if err := node.Client.ContainerStart(context.WithoutCancel(ctx), stopped[i],
				container.StartOptions{}); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", stopped[i], err))
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("restarting the stopped containers: %s — they were stopped for the snapshot and MUST be checked", strings.Join(errs, "; "))
		}
		return nil
	}

	for _, name := range names {
		if err := node.Client.ContainerStop(ctx, name, container.StopOptions{}); err != nil {
			_ = restart()
			return nil, fmt.Errorf("stopping %s for a consistent snapshot: %w — the backup was not taken", name, err)
		}
		stopped = append(stopped, name)
	}
	return restart, nil
}

// restoreVolume is the volume half of handleRestore: an already-decrypted tar on the
// request body, streamed into the volume. Two refusals, both named for the operator:
//
//   - In use ⇒ refused, listing the containers. Dokploy force-removes an unused volume on
//     restore; Daffa refuses and explains — restoring over a live consumer corrupts
//     exactly the data the backup existed to protect.
//   - Non-empty ⇒ refused without ?wipe=1. A restore into a non-empty volume merges, and
//     a merge of two data states is garbage that only reveals itself later. The wipe is
//     explicit, audited, and impossible to trigger from a form by accident.
func (s *Server) restoreVolume(w http.ResponseWriter, r *http.Request, job *store.BackupJob) {
	ctx := r.Context()

	node, err := s.volumeNode(ctx, job.EnvID, job.Volume, true)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "env_unreachable", err.Error())
		return
	}

	users, err := volumeUsers(ctx, node, job.Volume)
	if err == nil && len(users) > 0 {
		httpx.Fail(w, r, http.StatusConflict, "volume_in_use",
			fmt.Sprintf("The volume is mounted by %s. Stop and remove those containers first — restoring under a live consumer corrupts exactly the data this backup exists to protect.",
				strings.Join(users, ", ")))
		return
	}

	wipe := r.URL.Query().Get("wipe") == "1"
	exists := true
	if _, verr := node.Client.VolumeInspect(ctx, job.Volume); verr != nil {
		exists = false // a fresh box; Restore creates it
	}
	if exists && wipe {
		if err := volumes.Wipe(ctx, node, job.Volume); err != nil {
			httpx.Fail(w, r, http.StatusBadGateway, "wipe_failed", err.Error())
			return
		}
	}
	if exists && !wipe {
		empty, err := volumes.IsEmpty(ctx, node, job.Volume)
		if err != nil {
			httpx.Fail(w, r, http.StatusBadGateway, "volume_unreadable", err.Error())
			return
		}
		if !empty {
			httpx.Fail(w, r, http.StatusConflict, "volume_not_empty",
				"The volume is not empty, and restoring into it would merge two states of the data — garbage that only shows up later. Pass --wipe to empty it first, explicitly.")
			return
		}
	}

	s.audit(ctx, store.AuditEntry{
		EnvID: job.EnvID, Action: "backup.restore", Target: job.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{"event": "started", "volume": job.Volume,
			"wiped": fmt.Sprintf("%t", exists && wipe)}),
	})

	// No body size limit, deliberately: this is someone's data, and capping it would
	// silently truncate the restore. The route is admin-only, like the database path.
	err = volumes.Restore(ctx, node, job.Volume, r.Body)

	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.audit(ctx, store.AuditEntry{
		EnvID: job.EnvID, Action: "backup.restore", Target: job.Name, Outcome: outcome,
		Detail: store.AuditDetail(map[string]string{"event": "finished", "volume": job.Volume, "error": errText(err)}),
	})

	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "restore_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// volumeUsers names every container — running or stopped — that mounts the volume. It is
// what lets a restore refusal say WHO, not just no.
func volumeUsers(ctx context.Context, node *dockerx.Node, volumeName string) ([]string, error) {
	list, err := node.Client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("volume", volumeName)),
	})
	if err != nil {
		return nil, fmt.Errorf("listing the volume's containers: %w", err)
	}
	var names []string
	for _, c := range list {
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		names = append(names, name)
	}
	return names, nil
}
