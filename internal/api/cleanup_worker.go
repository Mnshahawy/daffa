package api

// Automatic disk cleanup: the cron that keeps a deploy host from filling up.
//
// A host Daffa deploys to accumulates three things forever unless somebody sweeps them: the
// superseded image of every release (still tagged, so a dangling prune never sees it), the
// stopped containers of previous deployments — swarm keeps the last few task containers per
// service, writable layers and all — and BuildKit's cache. None of that is Daffa's to keep,
// but on the box it manages, it is Daffa's to offer to remove.
//
// The difference between this and `docker system prune -a` is the age floor. See
// .ai/cleanup.md.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// cleanupRunTimeout bounds one host's sweep. Pruning a few thousand images on a busy
// daemon is minutes, not hours — a sweep still going after this is one waiting on a wedged
// daemon, and holding the per-host lock forever would mean the host is never swept again.
const cleanupRunTimeout = time.Hour

// cleanupScheduler runs the effective cleanup policy of each host on its cron. It is
// rebuilt from the database whenever a policy changes, for the same reason the backup
// scheduler is: a rebuild cannot drift from what the database says.
type cleanupScheduler struct {
	mu   sync.Mutex
	cron *cron.Cron

	// running is the set of env ids with a sweep in flight. Two prunes of the same daemon
	// at once (an overlapping cron fire, or "Run now" landing on top of a scheduled sweep)
	// is contention on the daemon for no gain — the second trigger is skipped, not queued.
	runMu   sync.Mutex
	running map[string]bool
}

func newCleanupScheduler() *cleanupScheduler {
	return &cleanupScheduler{running: map[string]bool{}}
}

func (c *cleanupScheduler) tryStart(envID string) bool {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.running[envID] {
		return false
	}
	c.running[envID] = true
	return true
}

func (c *cleanupScheduler) finish(envID string) {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	delete(c.running, envID)
}

// rebuildCleanupSchedule reloads every host's effective policy and rebuilds the cron.
// Called at startup and after any policy change, so an edited schedule is live now rather
// than at the next restart.
func (s *Server) rebuildCleanupSchedule(ctx context.Context) {
	envs, err := s.store.ListEnvironments(ctx)
	if err != nil {
		slog.Error("loading environments for the cleanup schedule", "err", err)
		return
	}

	s.cleanup.mu.Lock()
	defer s.cleanup.mu.Unlock()

	if s.cleanup.cron != nil {
		s.cleanup.cron.Stop()
	}
	// UTC, like the backup schedules: a sweep that silently shifts twice a year with the
	// server's timezone is a sweep nobody can reason about.
	c := cron.New(cron.WithLocation(time.UTC))

	scheduled := 0
	for _, env := range envs {
		policy, err := s.store.EffectiveCleanupPolicy(ctx, env.ID)
		if err != nil {
			slog.Error("loading a cleanup policy", "env", env.Name, "err", err)
			continue
		}
		if policy == nil || !policy.Enabled || policy.Schedule == "" {
			continue
		}
		envID := env.ID
		if _, err := c.AddFunc(policy.Schedule, func() {
			// Not the worker context: a sweep that started must finish its prune rather
			// than be cancelled halfway through by a shutdown. The timeout is its bound.
			runCtx, cancel := context.WithTimeout(context.Background(), cleanupRunTimeout)
			defer cancel()
			s.runCleanup(runCtx, envID, "schedule")
		}); err != nil {
			slog.Error("invalid cleanup schedule", "env", env.Name, "schedule", policy.Schedule, "err", err)
			continue
		}
		scheduled++
	}

	c.Start()
	s.cleanup.cron = c
	slog.Info("cleanup schedule loaded", "hosts", scheduled)
}

func (s *Server) stopCleanupScheduler() {
	s.cleanup.mu.Lock()
	defer s.cleanup.mu.Unlock()
	if s.cleanup.cron != nil {
		s.cleanup.cron.Stop()
	}
}

// cleanupPruneTarget maps a policy target to the Docker prune it performs. The store names
// these in its own strings on purpose (it does not know what Docker is); this is the one
// place the two vocabularies meet, and TestCleanupTargetsAllMapToPrunes pins it.
func cleanupPruneTarget(t store.CleanupTarget) (dockerx.PruneTarget, bool) {
	switch t {
	case store.CleanupImages:
		return dockerx.PruneImages, true
	case store.CleanupContainers:
		return dockerx.PruneContainers, true
	case store.CleanupNetworks:
		return dockerx.PruneNetworks, true
	case store.CleanupBuildCache:
		return dockerx.PruneBuildCache, true
	}
	return "", false
}

// runCleanup sweeps one host and records what it reclaimed.
//
// Errors are recorded, not returned: this runs from cron, which has nobody to return them
// to, and from a handler that has already answered. The run record IS the report.
func (s *Server) runCleanup(ctx context.Context, envID, trigger string) {
	// One sweep per host at a time, claimed before the run record exists so two triggers
	// cannot both open one.
	if !s.cleanup.tryStart(envID) {
		slog.Info("cleanup already in progress; skipping this trigger", "env", envID, "trigger", trigger)
		return
	}
	defer s.cleanup.finish(envID)

	policy, err := s.store.EffectiveCleanupPolicy(ctx, envID)
	if err != nil {
		slog.Error("loading the cleanup policy", "env", envID, "err", err)
		return
	}
	// A policy switched off between the cron firing and this running is a policy switched
	// off. A manual run is the operator asking for this sweep now, and needs only the
	// targets — but a policy that names none has nothing to do either way.
	if policy == nil || len(policy.Targets) == 0 || (!policy.Enabled && trigger != "manual") {
		return
	}

	if err := s.store.StartCleanupRun(ctx, envID, trigger); err != nil {
		slog.Error("recording the cleanup run", "env", envID, "err", err)
		return
	}

	freed, deleted, runErr := s.sweepHost(ctx, envID, policy)

	if err := s.store.FinishCleanupRun(ctx, envID, freed, deleted, runErr); err != nil {
		slog.Error("recording the cleanup outcome", "env", envID, "err", err)
	}

	outcome := "ok"
	detail := map[string]any{
		"trigger": trigger, "freed": freed, "deleted": deleted,
		"keep_hours": policy.KeepHours, "targets": joinCleanupTargets(policy.Targets),
	}
	if runErr != nil {
		outcome = "error"
		detail["error"] = runErr.Error()
	}
	// Audited like the manual prune, because it is the same act: something deleted things
	// on a host, and the log has to say what and how much.
	s.audit(ctx, store.AuditEntry{
		EnvID: envID, Action: "cleanup.run", Target: envID, Outcome: outcome,
		Detail: store.AuditDetail(detail),
	})
	slog.Info("cleanup finished", "env", envID, "trigger", trigger,
		"freed", freed, "deleted", deleted, "err", runErr)
}

// sweepHost prunes every target on every node of the host.
//
// Every node, not just one: disk fills per machine, and a swarm's stopped task containers
// are on whichever node ran them. This is node-local work that fans out, which is what
// Env.Nodes() is for (see dockerx's package doc).
func (s *Server) sweepHost(ctx context.Context, envID string, policy *store.CleanupPolicy) (freed int64, deleted int, runErr error) {
	env, err := s.pool.Get(envID)
	if err != nil {
		return 0, 0, fmt.Errorf("this host is not connected: %w", err)
	}
	nodes := env.Nodes()
	if len(nodes) == 0 {
		return 0, 0, errors.New("this host has no reachable node")
	}

	opts := dockerx.PruneOptions{
		MinAge: time.Duration(policy.KeepHours) * time.Hour,
		// The point of the scheduled sweep: superseded release images are still TAGGED,
		// so a dangling-only prune walks straight past the thing that is actually filling
		// the disk. The age floor above is what makes this safe to do unattended.
		UnusedImages: true,
	}

	var failures []string
	for _, node := range nodes {
		for _, target := range policy.Targets {
			pt, ok := cleanupPruneTarget(target)
			if !ok {
				continue // unknown target in the database; the policy validator refuses new ones
			}
			res, err := node.PruneWith(ctx, pt, opts)
			if err != nil {
				// One failing target does not abandon the rest: reclaiming three of four
				// kinds beats reclaiming none because the build cache was busy.
				failures = append(failures, fmt.Sprintf("%s on %s: %v", target, node.Name, err))
				continue
			}
			freed += int64(res.Freed)
			deleted += res.Deleted
		}
	}
	if len(failures) > 0 {
		return freed, deleted, errors.New(strings.Join(failures, "; "))
	}
	return freed, deleted, nil
}

func joinCleanupTargets(targets []store.CleanupTarget) string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, string(t))
	}
	return strings.Join(out, " ")
}
