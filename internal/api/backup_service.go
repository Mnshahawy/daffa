package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Mnshahawy/daffa/internal/backups"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// scheduler runs enabled backup jobs on their cron expressions. It is rebuilt from the
// database whenever a job changes, rather than mutated in place — the set of jobs is
// tiny, and a rebuild cannot drift from what the database says, which a series of
// incremental add/remove calls eventually would.
type scheduler struct {
	mu   sync.Mutex
	cron *cron.Cron

	// running is the set of job ids with a backup in flight, so a job never runs concurrently
	// with itself. robfig/cron fires each entry in its own goroutine and does not skip a run
	// whose predecessor is still going, and a manual "Run now" can land on top of a scheduled
	// one — either way two `pg_dumpall`s into the same container is doubled load for no gain.
	// The guard coalesces both cases: the second trigger is skipped, not queued.
	runMu   sync.Mutex
	running map[string]bool
}

func newScheduler() *scheduler {
	return &scheduler{running: map[string]bool{}}
}

// tryStart claims the in-flight slot for a job, returning false if one is already held.
func (s *scheduler) tryStart(jobID string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.running[jobID] {
		return false
	}
	s.running[jobID] = true
	return true
}

func (s *scheduler) finish(jobID string) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	delete(s.running, jobID)
}

func (s *Server) startScheduler(ctx context.Context) {
	// A backup that was in flight when the process died is not still running. Saying it
	// is would be the most dangerous lie this feature could tell.
	if err := s.store.FailStaleBackupRuns(ctx); err != nil {
		slog.Error("clearing stale backup runs", "err", err)
	}
	s.rebuildSchedule(ctx)
}

func (s *Server) rebuildSchedule(ctx context.Context) {
	// The scheduler runs on behalf of the system, not a user: every job, regardless of who
	// can see it.
	jobs, err := s.store.AllBackupJobs(ctx)
	if err != nil {
		slog.Error("loading backup jobs", "err", err)
		return
	}

	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()

	if s.sched.cron != nil {
		s.sched.cron.Stop()
	}
	// Cron expressions are interpreted in UTC. A schedule that silently shifts twice a
	// year with the server's timezone is a schedule nobody can reason about.
	c := cron.New(cron.WithLocation(time.UTC))

	scheduled := 0
	for _, job := range jobs {
		if !job.Enabled || job.Schedule == "" {
			continue
		}
		jobID := job.ID
		if _, err := c.AddFunc(job.Schedule, func() {
			s.runBackup(context.Background(), jobID, "schedule", "")
		}); err != nil {
			slog.Error("invalid backup schedule", "job", job.Name, "schedule", job.Schedule, "err", err)
			continue
		}
		scheduled++
	}

	c.Start()
	s.sched.cron = c
	slog.Info("backup schedule loaded", "jobs", scheduled)
}

func (s *Server) stopScheduler() {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	if s.sched.cron != nil {
		s.sched.cron.Stop()
	}
}

// runBackup performs one backup and records it. Errors are recorded, not returned: this
// runs both from an HTTP handler (which has already replied) and from cron (which has
// nobody to reply to). The run record IS the report.
func (s *Server) runBackup(ctx context.Context, jobID, trigger, userID string) {
	// One run per job at a time. A second trigger (an overlapping cron fire, or a manual run on
	// top of a scheduled one) is coalesced away rather than doubled up. Claimed before the run
	// record exists, so two triggers cannot both open a BackupRun row.
	if !s.sched.tryStart(jobID) {
		slog.Info("backup already in progress; skipping this trigger", "job", jobID, "trigger", trigger)
		return
	}
	defer s.sched.finish(jobID)

	job, err := s.store.BackupJobByID(ctx, jobID)
	if err != nil {
		slog.Error("backup job vanished", "job", jobID, "err", err)
		return
	}

	run := &store.BackupRun{JobID: job.ID, Trigger: trigger, StartedBy: userID}
	if err := s.store.StartBackupRun(ctx, run); err != nil {
		slog.Error("recording the backup run", "job", job.Name, "err", err)
		return
	}

	// A backup can legitimately take a long time; it must not inherit a request's
	// context, or closing the browser tab would kill it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Hour)
	defer cancel()

	result, err := s.performBackup(ctx, job)

	var bytes int64
	var key string
	if result != nil {
		bytes, key = result.Bytes, result.ObjectKey
	}
	if ferr := s.store.FinishBackupRun(ctx, run.ID, bytes, key, err); ferr != nil {
		slog.Error("recording the backup result", "job", job.Name, "err", ferr)
	}

	// The chokepoint every backup outcome passes through — scheduled or manual. This is the
	// notification the whole feature exists for: a backup that has been failing quietly
	// every night is the thing you find out about on the day you need the backup.
	s.notifyBackup(ctx, job, bytes, err)

	outcome := "ok"
	detail := map[string]any{"bytes": bytes, "object": key, "trigger": trigger}
	if err != nil {
		outcome = "error"
		detail["error"] = err.Error()
		slog.Error("backup failed", "job", job.Name, "err", err)
	} else {
		slog.Info("backup complete", "job", job.Name, "bytes", bytes, "object", key)
	}

	_ = s.store.Audit(ctx, store.AuditEntry{
		UserID: userID, EnvID: job.EnvID, Action: "backup.run", Target: job.Name,
		Outcome: outcome, Detail: store.AuditDetail(detail),
	})
}

func (s *Server) performBackup(ctx context.Context, job *store.BackupJob) (*backups.Result, error) {
	if job.Engine == string(backups.Volume) {
		return s.performVolumeBackup(ctx, job)
	}

	node, containerName, err := s.backupNode(ctx, job.EnvID, job.Container)
	if err != nil {
		return nil, err
	}

	spec, dst, err := s.jobConfig(ctx, job)
	if err != nil {
		return nil, err
	}
	return backups.Run(ctx, node, containerName, spec, dst, time.Now())
}

// backupNode is the daemon a backup job's container actually lives on.
//
// A backup is an EXEC into the database container, and exec is node-local: no manager can exec into
// a container on another machine. So a job needs a NODE, not an environment — and on a Swarm the
// scheduler decided which one, possibly since the last backup ran.
//
// So it is LOOKED UP, every time, rather than remembered. A swarm task's container is recreated on
// a different machine whenever it is rescheduled, and a node id stored on the job would be a fact
// that quietly stopped being true — the worst kind, because the backup would keep succeeding
// against nothing.
//
// It REFUSES rather than guesses when it cannot find the container. Picking a node and hoping is
// how you back up an empty database and find out months later, from the wrong side.
func (s *Server) backupNode(ctx context.Context, envID, containerRef string) (*dockerx.Node, string, error) {
	env, err := s.pool.Get(envID)
	if err != nil {
		return nil, "", fmt.Errorf("the environment for this job is not connected")
	}

	// One node, and not a swarm: nothing to resolve, and the ref is already a container. Asking the
	// daemon for a list to learn what we already know would be a round trip for nothing — and if the
	// container is not there, the exec says so far more precisely than this could.
	if node, err := env.One(); err == nil && !env.IsSwarm() {
		return node, containerRef, nil
	}

	// A swarm, or several nodes. Find the machine holding the container — and the container's REAL
	// name, which is not the one the job was written with.
	for _, node := range env.Nodes() {
		list, err := node.ListContainers(ctx, false)
		if err != nil {
			continue // that machine is down; the container may still be on another
		}
		for _, c := range list {
			if c.Name == containerRef || strings.HasPrefix(c.ID, containerRef) {
				return node, c.Name, nil
			}
			// A swarm task's container is named `<stack>_<service>.<slot>.<taskid>` — a name nobody
			// would write down, and one that CHANGES on every reschedule. So a job names the
			// SERVICE, and the real container is resolved here, freshly, on every run. Storing the
			// task name on the job would be storing a fact that quietly stops being true, which is
			// the worst kind: the backup would keep succeeding against a container that no longer
			// exists.
			if strings.HasPrefix(c.Name, containerRef+".") {
				return node, c.Name, nil
			}
		}
	}

	return nil, "", fmt.Errorf("no running container matching %q on any node Daffa can reach in this environment", containerRef)
}

// jobConfig unseals a job's secrets and resolves its storage target. The plaintext exists
// only for the duration of the operation that needs it.
func (s *Server) jobConfig(ctx context.Context, job *store.BackupJob) (backups.Spec, backups.Destination, error) {
	var none backups.Destination

	dbPass, err := s.sealer.Open(job.DBPasswordEnc)
	if err != nil {
		return backups.Spec{}, none, fmt.Errorf("could not decrypt the database password (was the master key replaced?)")
	}

	target, err := s.store.StorageTargetByID(ctx, job.StorageID)
	if err != nil {
		return backups.Spec{}, none, fmt.Errorf("the storage target for this job no longer exists")
	}
	secret, err := s.sealer.Open(target.SecretEnc)
	if err != nil {
		return backups.Spec{}, none, fmt.Errorf("could not decrypt the credential for %s (was the master key replaced?)", target.Name)
	}

	spec := backups.Spec{
		Engine:    backups.Engine(job.Engine),
		Databases: job.Databases,
		User:      job.DBUser,
		Password:  dbPass,
	}
	// Resolve the job's keys to recipient strings here, at the seam: the pipeline still
	// just receives public keys and stays ignorant of key management.
	var recipients string
	if job.Encrypted() {
		recs, err := s.store.JobRecipients(ctx, job.ID)
		if err != nil {
			return backups.Spec{}, none, fmt.Errorf("could not resolve the encryption keys for this job: %w", err)
		}
		if len(recs) == 0 {
			return backups.Spec{}, none, fmt.Errorf("this job is set to encrypt but has no encryption keys — add one before running it")
		}
		recipients = strings.Join(recs, " ")
	}

	dst := backups.Destination{
		Endpoint:   target.Endpoint,
		Region:     target.Region,
		Bucket:     target.Bucket,
		Prefix:     job.Prefix,
		KeyID:      target.KeyID,
		Secret:     secret,
		Encrypt:    job.Encrypted(),
		Recipients: recipients,
	}
	return spec, dst, nil
}

// notifyBackup tells whoever asked. Never fails the run.
func (s *Server) notifyBackup(ctx context.Context, job *store.BackupJob, written int64, runErr error) {
	failed := runErr != nil

	event := notify.BackupSucceeded
	if failed {
		event = notify.BackupFailed
	}

	host := s.envName(ctx, job.EnvID)
	verb := map[bool]string{true: "failed", false: "succeeded"}[failed]

	d := notify.Data{
		Event:    event,
		Subject:  fmt.Sprintf("Backup %s: %s on %s", verb, job.Name, host),
		Title:    fmt.Sprintf("Backup %s: %s", verb, job.Name),
		HostName: host,
		Target:   job.Name,
		Link:     "/backups",
		Failed:   failed,
	}

	if failed {
		d.Summary = fmt.Sprintf("The backup job %q on %s failed.", job.Name, host)
		d.Detail = notify.Tail(runErr.Error(), 12, 2000)
	} else {
		d.Summary = fmt.Sprintf("The backup job %q on %s completed. %s written.",
			job.Name, host, humanBytes(written))
	}

	s.notify.Send(ctx, job.EnvID, d)
}

// humanBytes renders a size the way an operator reads it. "412 MB" beats "432012800".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
