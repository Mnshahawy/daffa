package store

// Automatic disk cleanup: a fleet-wide default and an optional per-host override, run by
// the cleanup worker on a cron. The shape mirrors the logging defaults (log_settings /
// env_log_configs) exactly, for the reasons written on that migration.
//
// What makes this safe enough to schedule is KeepHours: an age floor under every prune,
// so the artifacts of the last few days — the image the previous release ran, the stopped
// container whose logs someone is about to read — are never in scope. See .ai/cleanup.md.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CleanupTarget names one kind of reclaimable artifact. These are the store's own strings
// rather than dockerx.PruneTarget values: the store does not know what Docker is. The
// mapping to the prune targets is pinned by a test in internal/api.
type CleanupTarget string

const (
	CleanupImages     CleanupTarget = "images"
	CleanupContainers CleanupTarget = "containers"
	CleanupNetworks   CleanupTarget = "networks"
	CleanupBuildCache CleanupTarget = "build-cache"
)

// CleanupTargets is every target a POLICY may name, in the order a run performs them:
// containers first, because a stopped container holds a reference to its image and
// pruning it first is what lets the image go in the same pass.
//
// Volumes are deliberately absent and must stay absent. A pruned image is a re-pull; a
// pruned volume is deleted data, and the difference between "anonymous" and "the database
// of a stack that happens to be stopped" is one label nobody checks at 03:00. Volumes are
// removed one at a time, by a human, from the volumes page.
var CleanupTargets = []CleanupTarget{CleanupContainers, CleanupImages, CleanupNetworks, CleanupBuildCache}

func ValidCleanupTarget(t CleanupTarget) bool {
	for _, v := range CleanupTargets {
		if v == t {
			return true
		}
	}
	return false
}

// CleanupPolicy is when to sweep a host, what to sweep, and how old a thing must be
// before the sweep may take it.
type CleanupPolicy struct {
	Enabled   bool            `json:"enabled"`
	Schedule  string          `json:"schedule"` // cron, UTC; required when enabled
	Targets   []CleanupTarget `json:"targets"`
	KeepHours int             `json:"keep_hours"` // artifacts younger than this are never touched
	UpdatedAt time.Time       `json:"updated_at"`
}

// ErrInvalidCleanupPolicy is every way a policy can be wrong: the REQUEST is wrong, so the
// API owes a 400 with the sentence. Same technique as ErrInvalidLogConfig.
var ErrInvalidCleanupPolicy = errors.New("store: not a cleanup policy a host can run")

type badCleanupPolicy struct{ msg string }

func (e badCleanupPolicy) Error() string        { return e.msg }
func (e badCleanupPolicy) Is(target error) bool { return target == ErrInvalidCleanupPolicy }

func invalidCleanup(format string, a ...any) error {
	return badCleanupPolicy{msg: fmt.Sprintf(format, a...)}
}

// maxKeepHours is a year. Past that the policy is not a retention window, it is a typo —
// and one that would quietly mean "never actually prune anything".
const maxKeepHours = 24 * 365

// Validate checks everything except the cron expression, which the API layer parses with
// the same library the scheduler uses (the store has no business importing a cron parser).
// A DISABLED policy is only checked for shape: someone switching the sweep off should not
// have to first fix the schedule they are switching off.
func (p *CleanupPolicy) Validate() error {
	if p.KeepHours < 0 || p.KeepHours > maxKeepHours {
		return invalidCleanup("Keep hours must be between 0 and %d (a year).", maxKeepHours)
	}
	seen := map[CleanupTarget]bool{}
	for _, t := range p.Targets {
		if t == "volumes" {
			return invalidCleanup("Volumes are never swept automatically — a pruned volume is deleted data, " +
				"not a re-pull. Remove a volume from the volumes page, one at a time.")
		}
		if !ValidCleanupTarget(t) {
			return invalidCleanup("%q is not something a cleanup can prune — pick from images, containers, "+
				"networks and build-cache.", t)
		}
		if seen[t] {
			return invalidCleanup("%q is named twice.", t)
		}
		seen[t] = true
	}
	if !p.Enabled {
		return nil
	}
	if strings.TrimSpace(p.Schedule) == "" {
		return invalidCleanup("A schedule is required to run the cleanup automatically (e.g. \"30 3 * * *\" for 03:30 UTC daily).")
	}
	if len(p.Targets) == 0 {
		return invalidCleanup("Choose at least one thing to prune, or the cleanup would run and do nothing.")
	}
	return nil
}

// normalize puts the targets in run order and trims the schedule, so two policies that
// mean the same thing are stored the same way.
func (p *CleanupPolicy) normalize() {
	p.Schedule = strings.TrimSpace(p.Schedule)
	sort.SliceStable(p.Targets, func(i, j int) bool {
		return cleanupTargetOrder(p.Targets[i]) < cleanupTargetOrder(p.Targets[j])
	})
}

func cleanupTargetOrder(t CleanupTarget) int {
	for i, v := range CleanupTargets {
		if v == t {
			return i
		}
	}
	return len(CleanupTargets)
}

const cleanupCols = `enabled, schedule, targets, keep_hours, updated_at`

// GlobalCleanupPolicy reads the fleet-wide default. Never configured returns (nil, nil):
// "no automatic cleanup" is a deliberate state, and inventing a default that deletes
// things on a host nobody asked about would be the wrong kind of helpful.
func (s *Store) GlobalCleanupPolicy(ctx context.Context) (*CleanupPolicy, error) {
	return scanCleanupPolicy(s.queryRow(ctx,
		`SELECT `+cleanupCols+` FROM cleanup_settings WHERE id = 'cleanup'`))
}

func (s *Store) SaveGlobalCleanupPolicy(ctx context.Context, p *CleanupPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	p.normalize()
	p.UpdatedAt = now()

	_, err := s.exec(ctx, `
        INSERT INTO cleanup_settings (id, enabled, schedule, targets, keep_hours, updated_at)
        VALUES ('cleanup', ?, ?, ?, ?, ?)
        ON CONFLICT (id) DO UPDATE SET
            enabled = EXCLUDED.enabled,
            schedule = EXCLUDED.schedule,
            targets = EXCLUDED.targets,
            keep_hours = EXCLUDED.keep_hours,
            updated_at = EXCLUDED.updated_at`,
		boolInt(p.Enabled), p.Schedule, joinTargets(p.Targets), p.KeepHours, ts(p.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: saving the cleanup policy: %w", err)
	}
	return nil
}

// DeleteGlobalCleanupPolicy unsets the fleet default. Idempotent: deleting what is already
// unset is the state the caller wanted.
func (s *Store) DeleteGlobalCleanupPolicy(ctx context.Context) error {
	if _, err := s.exec(ctx, `DELETE FROM cleanup_settings WHERE id = 'cleanup'`); err != nil {
		return fmt.Errorf("store: clearing the cleanup policy: %w", err)
	}
	return nil
}

// EnvCleanupPolicy reads one host's override. (nil, nil) when the host follows the fleet
// default.
func (s *Store) EnvCleanupPolicy(ctx context.Context, envID string) (*CleanupPolicy, error) {
	return scanCleanupPolicy(s.queryRow(ctx,
		`SELECT `+cleanupCols+` FROM env_cleanup_policies WHERE env_id = ?`, envID))
}

func (s *Store) SaveEnvCleanupPolicy(ctx context.Context, envID string, p *CleanupPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	p.normalize()
	p.UpdatedAt = now()

	_, err := s.exec(ctx, `
        INSERT INTO env_cleanup_policies (env_id, enabled, schedule, targets, keep_hours, updated_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT (env_id) DO UPDATE SET
            enabled = EXCLUDED.enabled,
            schedule = EXCLUDED.schedule,
            targets = EXCLUDED.targets,
            keep_hours = EXCLUDED.keep_hours,
            updated_at = EXCLUDED.updated_at`,
		envID, boolInt(p.Enabled), p.Schedule, joinTargets(p.Targets), p.KeepHours, ts(p.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: saving the cleanup policy for %s: %w", envID, err)
	}
	return nil
}

// DeleteEnvCleanupPolicy reverts a host to the fleet default.
func (s *Store) DeleteEnvCleanupPolicy(ctx context.Context, envID string) error {
	if _, err := s.exec(ctx, `DELETE FROM env_cleanup_policies WHERE env_id = ?`, envID); err != nil {
		return fmt.Errorf("store: clearing the cleanup policy for %s: %w", envID, err)
	}
	return nil
}

// EffectiveCleanupPolicy is what the worker actually runs on this host: the host's
// override if there is one, else the fleet default, else nil. The precedence lives here,
// exactly once — the worker calls this and nothing else.
//
// An override wins even when it is DISABLED: "this host does not get swept" is the point
// of overriding a fleet-wide sweep.
func (s *Store) EffectiveCleanupPolicy(ctx context.Context, envID string) (*CleanupPolicy, error) {
	p, err := s.EnvCleanupPolicy(ctx, envID)
	if err != nil || p != nil {
		return p, err
	}
	return s.GlobalCleanupPolicy(ctx)
}

func joinTargets(targets []CleanupTarget) string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, string(t))
	}
	return strings.Join(out, " ")
}

func splitTargets(s string) []CleanupTarget {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	out := make([]CleanupTarget, 0, len(fields))
	for _, f := range fields {
		out = append(out, CleanupTarget(f))
	}
	return out
}

func scanCleanupPolicy(row scanner) (*CleanupPolicy, error) {
	var (
		p         CleanupPolicy
		enabled   int
		targets   string
		updatedAt string
	)
	err := row.Scan(&enabled, &p.Schedule, &targets, &p.KeepHours, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading a cleanup policy: %w", err)
	}
	p.Enabled = enabled != 0
	p.Targets = splitTargets(targets)
	p.UpdatedAt = parseTS(updatedAt)
	return &p, nil
}

// ── the last run ──────────────────────────────────────────────────────────────────

// CleanupRun is the last sweep of one host. One row per host, overwritten each run: the
// question it answers is "is this still running, and did it work?". The per-target detail
// of every run is in the audit log, which is where a history belongs.
type CleanupRun struct {
	EnvID     string    `json:"env_id"`
	Trigger   string    `json:"trigger"` // schedule | manual
	StartedAt time.Time `json:"started_at"`
	// Absent while a sweep is in flight — a run with no end is one that is still going,
	// or one the process died during. Both are things the UI must be able to say.
	EndedAt *time.Time `json:"ended_at,omitempty"`
	Freed   int64      `json:"freed"`
	Deleted int        `json:"deleted"`
	Error   string     `json:"error,omitempty"`
}

// StartCleanupRun records that a sweep began, replacing the previous run's record. A run
// that never finishes therefore shows as started-with-no-end rather than as the last
// successful one — the same "do not lie about being fine" posture as backup runs.
func (s *Store) StartCleanupRun(ctx context.Context, envID, trigger string) error {
	_, err := s.exec(ctx, `
        INSERT INTO env_cleanup_runs (env_id, trigger, started_at, ended_at, freed, deleted, error)
        VALUES (?, ?, ?, NULL, 0, 0, '')
        ON CONFLICT (env_id) DO UPDATE SET
            trigger = EXCLUDED.trigger,
            started_at = EXCLUDED.started_at,
            ended_at = NULL,
            freed = 0,
            deleted = 0,
            error = ''`,
		envID, trigger, ts(now()))
	if err != nil {
		return fmt.Errorf("store: starting the cleanup run for %s: %w", envID, err)
	}
	return nil
}

// FinishCleanupRun records the outcome. Freed and deleted are what the sweep actually
// reclaimed, which is the number that tells an operator whether the policy is doing
// anything at all.
func (s *Store) FinishCleanupRun(ctx context.Context, envID string, freed int64, deleted int, runErr error) error {
	var errText string
	if runErr != nil {
		errText = runErr.Error()
	}
	_, err := s.exec(ctx, `UPDATE env_cleanup_runs
        SET ended_at = ?, freed = ?, deleted = ?, error = ? WHERE env_id = ?`,
		ts(now()), freed, deleted, errText, envID)
	if err != nil {
		return fmt.Errorf("store: finishing the cleanup run for %s: %w", envID, err)
	}
	return nil
}

// LastCleanupRun returns (nil, nil) for a host that has never been swept.
func (s *Store) LastCleanupRun(ctx context.Context, envID string) (*CleanupRun, error) {
	var (
		r         CleanupRun
		startedAt string
		endedAt   sql.NullString
	)
	err := s.queryRow(ctx, `SELECT env_id, trigger, started_at, ended_at, freed, deleted, error
        FROM env_cleanup_runs WHERE env_id = ?`, envID).
		Scan(&r.EnvID, &r.Trigger, &startedAt, &endedAt, &r.Freed, &r.Deleted, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading the cleanup run for %s: %w", envID, err)
	}
	r.StartedAt = parseTS(startedAt)
	if endedAt.Valid {
		t := parseTS(endedAt.String)
		r.EndedAt = &t
	}
	return &r, nil
}
