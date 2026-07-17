package store

// Resource monitors: the settings, the rules, and the alerts they raise.
// See docs/monitoring.md. The samples themselves live in metrics.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxRetentionDays caps how long samples are kept.
//
// Not a whim, and not a CHECK constraint — a sentence explains it and a constraint violation
// does not. SQLite has no partitioning, so a read unions the day tables in range, and
// SQLITE_MAX_COMPOUND_SELECT is 500 terms by default; planning a union across hundreds of
// tables is not free either. Daffa is a container console, not a time-series database. If you
// want a year of history, ship the samples somewhere that wants them.
const MaxRetentionDays = 90

// MinIntervalSecs stops somebody setting a five-second interval across two hundred containers
// and wondering why their database is busy.
//
// 30 seconds, which is also the default. The cost of a round is not one query — it is one
// `docker stats` call per container per host, and one row written per container per round. At
// ten seconds and two hundred containers that is twenty inspections a second against the daemon,
// for ever, and 1.7 million rows a day to write, index and later drop; the sampler ends up
// costing more CPU than the things it is watching, which is a very silly way for a monitoring
// feature to fail.
//
// Nothing is gained for it either. A rule is written as "over the line for the WHOLE window",
// and the shortest window Daffa allows is 60 seconds — so a ten-second interval buys six samples
// where two already settle the question, and CPU is a counter delta averaged across the interval
// rather than an instantaneous reading, so the finer grain does not even reveal a shorter spike.
const MinIntervalSecs = 30

// These two are surfaced to the person who tripped them, VERBATIM, in the API's 400 — so they
// are written as sentences a user reads, not as "store: ..." log lines. The store-scoped errors
// elsewhere in this file stay 500s and keep their package prefix; these do not, on purpose.
var (
	ErrRetentionTooLong = errors.New("Retention is longer than Daffa keeps samples for")
	ErrIntervalTooShort = errors.New("Sampling that often would cost more than it tells you")
)

// MonitorSettings is how often we sample and how long we keep it. One row.
type MonitorSettings struct {
	Enabled       bool      `json:"enabled"`
	IntervalSecs  int       `json:"interval_secs"`
	RetentionDays int       `json:"retention_days"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (m MonitorSettings) Interval() time.Duration {
	return time.Duration(m.IntervalSecs) * time.Second
}

// MonitorSettings reads them. Never configured is a normal state, not an error: a fresh
// install samples every 30 seconds and keeps a week, and every caller would otherwise carry
// the same three lines of special-casing.
func (s *Store) MonitorSettings(ctx context.Context) (*MonitorSettings, error) {
	m := &MonitorSettings{Enabled: true, IntervalSecs: 30, RetentionDays: 7}

	var (
		enabled   int
		updatedAt string
	)
	err := s.queryRow(ctx,
		`SELECT enabled, interval_secs, retention_days, updated_at
         FROM monitor_settings WHERE id = 'monitoring'`,
	).Scan(&enabled, &m.IntervalSecs, &m.RetentionDays, &updatedAt)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return m, nil
	case err != nil:
		return nil, fmt.Errorf("store: reading monitor settings: %w", err)
	}

	m.Enabled = enabled == 1
	m.UpdatedAt = parseTS(updatedAt)

	// A row written before the floor was raised can hold a value the floor now forbids. Read it
	// UP to the floor rather than handing it back as it stands: nothing re-validates on the way
	// out, so an install configured at ten seconds would otherwise go on sampling every ten
	// seconds for ever — and its settings page would load that ten, then refuse to save any
	// change at all, retention included, citing an interval the person never touched.
	if m.IntervalSecs < MinIntervalSecs {
		m.IntervalSecs = MinIntervalSecs
	}
	return m, nil
}

func (s *Store) SaveMonitorSettings(ctx context.Context, m *MonitorSettings) error {
	if m.RetentionDays < 1 || m.RetentionDays > MaxRetentionDays {
		return fmt.Errorf("%w: %d days (the maximum is %d)",
			ErrRetentionTooLong, m.RetentionDays, MaxRetentionDays)
	}
	if m.IntervalSecs < MinIntervalSecs {
		return fmt.Errorf("%w: %ds is too short — the minimum is %ds, and it is also the default",
			ErrIntervalTooShort, m.IntervalSecs, MinIntervalSecs)
	}

	enabled := 0
	if m.Enabled {
		enabled = 1
	}

	// Stamped here, not taken from the caller — and written back onto the struct, so the handler
	// returns the row as it now IS rather than as it arrived, with a zero time where the
	// timestamp should be.
	m.UpdatedAt = now()

	// UPSERT, spelled the way both dialects accept.
	_, err := s.exec(ctx, `
        INSERT INTO monitor_settings (id, enabled, interval_secs, retention_days, updated_at)
        VALUES ('monitoring', ?, ?, ?, ?)
        ON CONFLICT (id) DO UPDATE SET
            enabled = EXCLUDED.enabled,
            interval_secs = EXCLUDED.interval_secs,
            retention_days = EXCLUDED.retention_days,
            updated_at = EXCLUDED.updated_at`,
		enabled, m.IntervalSecs, m.RetentionDays, ts(m.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: saving monitor settings: %w", err)
	}
	return nil
}

// ── the rules ───────────────────────────────────────────────────────────────────

// The metrics a rule can be written against, and the SQL that computes each. A rule's metric
// arrives as a string from the wire and ends up in a SQL fragment, so it is resolved through
// this map and NEVER interpolated — the difference between a column name and an injection is
// exactly this switch.
//
// Each resource is here TWICE, once as a share of what the container is allowed and once as an
// absolute quantity, because the two answer different questions and only one of them is always
// answerable. A percentage needs a limit to be a percentage OF; a container without one is
// allowed the whole machine, so `mem_pct > 80` silently becomes "80% of the host's RAM" — a
// line one container will essentially never cross, however badly it is behaving. The absolute
// metrics are the ones that work on an unlimited container, and they are why they exist.
var metricColumns = map[string]string{
	"cpu_pct":   "cpu_pct",
	"mem_pct":   "mem_pct",
	"mem_bytes": "mem_bytes",

	// CPU in cores USED — which is emphatically not the cpu_cores column. That column holds
	// what the container is ALLOWED (monitor.Collector writes it from dockerx.CPULimit), and it
	// is the denominator cpu_pct is a percentage of. Usage is therefore the allowance times the
	// share of it in use. Deriving it here rather than storing a fourth column means no
	// migration, and it means every sample already on disk can answer the question.
	"cpu_cores": "(cpu_pct * cpu_cores / 100.0)",
}

// PercentMetric reports whether a metric's threshold is a percentage. It decides two things: the
// valid range for a threshold (0..100, versus "any positive quantity"), and whether the rule
// depends on the container having a limit at all.
func PercentMetric(metric string) bool {
	return metric == "cpu_pct" || metric == "mem_pct"
}

// The comparisons. Same reasoning, same defence.
var comparisons = map[string]string{">": ">", "<": "<"}

var (
	// ErrInvalidMonitor is every way a rule can be wrong: a blank name, a five-second window, a
	// threshold of 300%, a metric that does not exist. They share a sentinel because they share
	// an answer — the REQUEST is wrong, not the server, so the API owes a 400 and the reason.
	//
	// Only the metric and the comparison used to carry one. Everything else fell through to a
	// 500 and "Something went wrong on our side" — which is a lie, told to somebody who has done
	// nothing worse than type the wrong number, and it buries the one sentence that would have
	// told them what the right number was.
	ErrInvalidMonitor = errors.New("store: not a rule a monitor can be written with")

	ErrUnknownMetric = errors.New("store: not a metric a monitor can watch")
	ErrBadComparison = errors.New("store: not a comparison a monitor can make")
)

// badRule is a validation failure, and its message is shown to the person who caused it —
// verbatim, in the API's 400. Hence the type rather than a %w chain: wrapping would prepend
// "not a rule a monitor can be written with: " to a sentence that already says what is wrong,
// and the reader would have to get past a restatement of the obvious to reach the useful half.
//
// It satisfies errors.Is(err, ErrInvalidMonitor) without carrying that text, and it still
// unwraps to the specific sentinel underneath when there is one.
type badRule struct {
	msg   string
	inner error // ErrUnknownMetric or ErrBadComparison, when the failure is one of those
}

func (e badRule) Error() string        { return e.msg }
func (e badRule) Is(target error) bool { return target == ErrInvalidMonitor }
func (e badRule) Unwrap() error        { return e.inner }

func invalid(format string, a ...any) error { return badRule{msg: fmt.Sprintf(format, a...)} }

// Monitor is one alert rule.
//
// EnvID, Stack and Container are filters, ANDed, and "" means "any". An empty EnvID therefore
// watches the WHOLE FLEET, which is why creating one takes monitors.edit globally — the API
// enforces that; see api/monitor_handlers.go.
//
// On the wire and in Go it is "", but in the database it is NULL — see nullEnv for why that
// distinction is load-bearing rather than cosmetic.
type Monitor struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Enabled      bool      `json:"enabled"`
	Metric       string    `json:"metric"`
	Op           string    `json:"op"`
	Threshold    float64   `json:"threshold"`
	DurationSecs int       `json:"duration_secs"`
	EnvID        string    `json:"env_id"`
	Stack        string    `json:"stack"`
	Container    string    `json:"container"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// validate. Every failure here is a person's mistake rather than a machine's, so every one of
// them wraps ErrInvalidMonitor — that is what buys the API a 400 with the reason in it, instead
// of a 500 that blames the server for a typo.
func (m *Monitor) validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return invalid("A monitor needs a name — it is what the alert will be called.")
	}
	if _, ok := metricColumns[m.Metric]; !ok {
		return badRule{fmt.Sprintf("%q is not a metric a monitor can watch.", m.Metric), ErrUnknownMetric}
	}
	if _, ok := comparisons[m.Op]; !ok {
		return badRule{fmt.Sprintf("%q is not a comparison a monitor can make.", m.Op), ErrBadComparison}
	}
	// A window shorter than a couple of samples cannot be "sustained" in any meaningful sense:
	// it would fire on a single reading, which is what a threshold with no duration is, and
	// then every transient spike pages somebody.
	if m.DurationSecs < 60 {
		return invalid("A monitor's window must be at least 60 seconds — anything shorter fires " +
			"on a single sample, and every passing spike becomes a page.")
	}
	if PercentMetric(m.Metric) {
		if m.Threshold < 0 || m.Threshold > 100 {
			return invalid("%s is a percentage, so %g is not a threshold it can cross.",
				metricLabel(m.Metric), m.Threshold)
		}
		return nil
	}
	// An absolute rule. Zero is not a threshold, it is a mistake with two failure modes: with
	// '>' it fires on everything for ever, and with '<' it fires on nothing, ever. Both look
	// like a working monitor from the outside.
	if m.Threshold <= 0 {
		return invalid("%s is measured as a quantity here, so its threshold must be above zero — "+
			"%g would match every sample, or none of them.", metricLabel(m.Metric), m.Threshold)
	}
	return nil
}

// metricLabel is the metric as the person writing the rule sees it. They chose "CPU" and "vCPU"
// from two dropdowns; they never typed "cpu_cores", and quoting it back at them is asking them to
// map our column names onto their screen before they can read their own mistake.
func metricLabel(metric string) string {
	if strings.HasPrefix(metric, "cpu") {
		return "CPU"
	}
	return "Memory"
}

const monitorCols = `id, name, enabled, metric, op, threshold, duration_secs,
    env_id, stack, container, created_at, updated_at`

func (s *Store) CreateMonitor(ctx context.Context, m *Monitor) error {
	if err := m.validate(); err != nil {
		return err
	}
	m.ID = "mon_" + NewID()
	m.CreatedAt, m.UpdatedAt = now(), now()

	_, err := s.exec(ctx, `INSERT INTO resource_monitors (`+monitorCols+`)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Name, boolInt(m.Enabled), m.Metric, m.Op, m.Threshold, m.DurationSecs,
		nullEnv(m.EnvID), m.Stack, m.Container, ts(m.CreatedAt), ts(m.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: creating monitor: %w", err)
	}
	return nil
}

func (s *Store) UpdateMonitor(ctx context.Context, m *Monitor) error {
	if err := m.validate(); err != nil {
		return err
	}
	m.UpdatedAt = now()

	res, err := s.exec(ctx, `UPDATE resource_monitors SET
        name = ?, enabled = ?, metric = ?, op = ?, threshold = ?, duration_secs = ?,
        env_id = ?, stack = ?, container = ?, updated_at = ?
        WHERE id = ?`,
		m.Name, boolInt(m.Enabled), m.Metric, m.Op, m.Threshold, m.DurationSecs,
		nullEnv(m.EnvID), m.Stack, m.Container, ts(m.UpdatedAt), m.ID)
	if err != nil {
		return fmt.Errorf("store: updating monitor: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	// Switching a monitor off must not leave its alerts firing forever. They are resolved with
	// a reason — and deliberately WITHOUT a notification, because nothing resolved: somebody
	// turned it off, and mailing "recovered" for that would be a small lie.
	if !m.Enabled {
		if err := s.resolveAlertsFor(ctx, m.ID, "the monitor was disabled"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) MonitorByID(ctx context.Context, id string) (*Monitor, error) {
	return scanMonitor(s.queryRow(ctx, `SELECT `+monitorCols+` FROM resource_monitors WHERE id = ?`, id))
}

func (s *Store) DeleteMonitor(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `DELETE FROM resource_monitors WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting monitor: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil // monitor_alerts cascades
}

// ListMonitors returns the monitors visible to a caller.
//
// It FILTERS; it does not gate. A host-scoped holder sees the monitors pinned to their host —
// and NOT the fleet-wide ones, which watch hosts they have no standing on.
//
// global short-circuits (an administrator sees everything, including the fleet-wide rules).
func (s *Store) ListMonitors(ctx context.Context, global bool, envs []string) ([]*Monitor, error) {
	q := `SELECT ` + monitorCols + ` FROM resource_monitors`
	var args []any

	if !global {
		if len(envs) == 0 {
			return nil, nil
		}
		ph := make([]string, len(envs))
		for i, e := range envs {
			ph[i] = "?"
			args = append(args, e)
		}
		q += ` WHERE env_id IN (` + strings.Join(ph, ",") + `)`
	}
	q += ` ORDER BY name`

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing monitors: %w", err)
	}
	defer rows.Close()

	var out []*Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// EnabledMonitors is what the evaluator runs each round.
func (s *Store) EnabledMonitors(ctx context.Context) ([]*Monitor, error) {
	rows, err := s.query(ctx,
		`SELECT `+monitorCols+` FROM resource_monitors WHERE enabled = 1 ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing enabled monitors: %w", err)
	}
	defer rows.Close()

	var out []*Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanMonitor(row scanner) (*Monitor, error) {
	var (
		m                    Monitor
		enabled              int
		envID                *string // NULL when the monitor watches every host
		createdAt, updatedAt string
	)
	err := row.Scan(&m.ID, &m.Name, &enabled, &m.Metric, &m.Op, &m.Threshold, &m.DurationSecs,
		&envID, &m.Stack, &m.Container, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading monitor: %w", err)
	}
	m.Enabled = enabled == 1
	if envID != nil {
		m.EnvID = *envID
	}
	m.CreatedAt, m.UpdatedAt = parseTS(createdAt), parseTS(updatedAt)
	return &m, nil
}

// nullEnv writes "every host" as NULL rather than as the empty string.
//
// The empty string would have to satisfy the foreign key to environments, and no host is
// called "" — so a fleet-wide monitor would be unstorable. NULL also makes the scoped filter
// right for free: `env_id IN (staging)` is NULL, not true, for a fleet-wide row, so a
// staging-scoped holder is not shown the rule that watches production.
func nullEnv(envID string) any {
	if envID == "" {
		return nil
	}
	return envID
}

// ── evaluation ──────────────────────────────────────────────────────────────────

// Breach is one container's behaviour across a monitor's window.
type Breach struct {
	EnvID         string
	ContainerName string
	ContainerID   string
	Stack         string

	Samples   int // how many readings the window actually contains
	Breaching int // how many of them crossed the threshold

	// Worst is the extreme in the BREACHING direction — the highest reading for a '>' rule,
	// the lowest for a '<'. It is what the alert reports.
	Worst float64
	// Best is the extreme in the RECOVERING direction, and it is what a resolution reports.
	// Quoting Worst in a recovery message produces "back within the threshold (100%)", which
	// is a sentence that contradicts itself and teaches an operator to distrust every other
	// number on the page.
	Best float64
}

// HasCoverage reports whether the window holds enough samples to say anything at all.
//
// This is not the same question as whether the rule held, and conflating the two is a bug I
// shipped and then watched happen: as a stopped container's samples aged out of the window,
// coverage collapsed, "sustained" went false, and that was read as RECOVERY — resolving the
// alert with the words "back within the threshold (100%)".
//
// Absence of evidence is not evidence of recovery. When coverage fails, the right answer is to
// say nothing and let the container's silence be handled as silence — see StaleAlerts.
func (b Breach) HasCoverage(expected int) bool {
	return b.Samples >= max(2, expected/2)
}

// Sustained reports whether the rule held for the WHOLE window.
//
// Every sample must breach — Prometheus's `for:` semantic, and the literal reading of "above
// 70% for ten minutes". One sample below the line resets the clock, and one below it again
// resolves the alert.
//
// The coverage floor is the other half, and it is not decoration. Without it, a host that was
// offline for the entire window and left one stale sample behind would satisfy "every sample in
// the window breached" — with a sample size of one — and page somebody about a machine that has
// not spoken in an hour.
func (b Breach) Sustained(expected int) bool {
	return b.HasCoverage(expected) && b.Breaching == b.Samples
}

// Evaluate runs one monitor over its window and returns a row per container in scope.
func (s *Store) Evaluate(ctx context.Context, m *Monitor, at time.Time) ([]Breach, error) {
	col, ok := metricColumns[m.Metric]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownMetric, m.Metric)
	}
	op, ok := comparisons[m.Op]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrBadComparison, m.Op)
	}

	from := at.Add(-time.Duration(m.DurationSecs) * time.Second)
	src, err := s.metricsSource(ctx, from, at)
	if err != nil {
		return nil, err
	}

	// The window is (from, at] — half-open at the bottom so a sample sitting exactly on the
	// boundary is not counted twice by two consecutive evaluations.
	where := []string{"ts_unix > ?", "ts_unix <= ?"}

	// ARGUMENT ORDER IS SQL-TEXT ORDER, not logical order. The threshold's placeholder lives in
	// the SELECT clause, which the database reads before the WHERE — so the threshold binds
	// FIRST, ahead of the window bounds, however unnatural that reads here. Get this wrong and
	// nothing errors: the threshold silently becomes a unix timestamp, every comparison is
	// false, and the monitor simply never fires. Which is the worst way for a monitor to fail.
	args := []any{m.Threshold, from.UTC().Unix(), at.UTC().Unix()}

	// Only the COLUMN and the OPERATOR are interpolated, and both come from the maps above —
	// never from the request. The threshold is bound.
	sel := fmt.Sprintf(`SUM(CASE WHEN %s %s ? THEN 1 ELSE 0 END)`, col, op)

	// Both extremes, because a firing alert and a resolving one want opposite ends of the
	// window. For '>', the worst is the highest reading and the best is the lowest; for '<' it
	// is the other way round. Reporting the maximum of a "CPU below 1%" alert would tell
	// somebody the number they were least interested in.
	worst, best := "MAX("+col+")", "MIN("+col+")"
	if op == "<" {
		worst, best = best, worst
	}

	if m.EnvID != "" {
		where = append(where, "env_id = ?")
		args = append(args, m.EnvID)
	}
	if m.Stack != "" {
		where = append(where, "stack = ?")
		args = append(args, m.Stack)
	}
	if m.Container != "" {
		where = append(where, "container_name = ?")
		args = append(args, m.Container)
	}

	q := fmt.Sprintf(`
        SELECT env_id, container_name, MAX(container_id), MAX(stack),
               COUNT(*), %s, %s, %s
        FROM %s
        WHERE %s
        GROUP BY env_id, container_name`,
		sel, worst, best, src, strings.Join(where, " AND "))

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: evaluating monitor %s: %w", m.Name, err)
	}
	defer rows.Close()

	var out []Breach
	for rows.Next() {
		var b Breach
		if err := rows.Scan(&b.EnvID, &b.ContainerName, &b.ContainerID, &b.Stack,
			&b.Samples, &b.Breaching, &b.Worst, &b.Best); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ── the alerts ──────────────────────────────────────────────────────────────────

type Alert struct {
	ID            string     `json:"id"`
	MonitorID     string     `json:"monitor_id"`
	MonitorName   string     `json:"monitor_name"`
	EnvID         string     `json:"env_id"`
	ContainerName string     `json:"container_name"`
	ContainerID   string     `json:"container_id"`
	Stack         string     `json:"stack"`
	State         string     `json:"state"`
	Value         float64    `json:"value"`
	StartedAt     time.Time  `json:"started_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	ResolvedAt    *time.Time `json:"resolved_at"`
	ResolveReason string     `json:"resolve_reason"`
}

const (
	AlertFiring   = "firing"
	AlertResolved = "resolved"
)

// FiringAlert finds the open alert for a monitor on a container, if there is one.
//
// Keyed on container_NAME. A compose name (billing-api-1) survives a redeploy and an id does
// not — an alert keyed on the id would reset its clock every deploy and never reach ten
// minutes, which for a memory leak that only shows up after an hour is precisely the alert you
// needed.
func (s *Store) FiringAlert(ctx context.Context, monitorID, containerName string) (*Alert, error) {
	var (
		a          Alert
		startedAt  string
		lastSeenAt string
	)
	err := s.queryRow(ctx, `SELECT id, monitor_id, env_id, container_name, container_id, stack,
        state, value, started_at, last_seen_at
        FROM monitor_alerts
        WHERE monitor_id = ? AND container_name = ? AND state = 'firing'`,
		monitorID, containerName,
	).Scan(&a.ID, &a.MonitorID, &a.EnvID, &a.ContainerName, &a.ContainerID, &a.Stack,
		&a.State, &a.Value, &startedAt, &lastSeenAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading a firing alert: %w", err)
	}
	a.StartedAt, a.LastSeenAt = parseTS(startedAt), parseTS(lastSeenAt)
	return &a, nil
}

func (s *Store) RaiseAlert(ctx context.Context, a *Alert) error {
	a.ID = "alr_" + NewID()
	a.State = AlertFiring
	a.StartedAt, a.LastSeenAt = now(), now()

	_, err := s.exec(ctx, `INSERT INTO monitor_alerts
        (id, monitor_id, env_id, container_name, container_id, stack, state, value,
         started_at, last_seen_at)
        VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.MonitorID, a.EnvID, a.ContainerName, a.ContainerID, a.Stack,
		a.State, a.Value, ts(a.StartedAt), ts(a.LastSeenAt))
	if err != nil {
		return fmt.Errorf("store: raising an alert: %w", err)
	}
	return nil
}

// TouchAlert keeps a firing alert's latest value current, so the UI shows what it is doing NOW
// and not what it was doing when it first tripped.
func (s *Store) TouchAlert(ctx context.Context, id string, value float64) error {
	_, err := s.exec(ctx,
		`UPDATE monitor_alerts SET value = ?, last_seen_at = ? WHERE id = ? AND state = 'firing'`,
		value, ts(now()), id)
	return err
}

func (s *Store) ResolveAlert(ctx context.Context, id, reason string) error {
	_, err := s.exec(ctx, `UPDATE monitor_alerts
        SET state = 'resolved', resolved_at = ?, resolve_reason = ?
        WHERE id = ? AND state = 'firing'`,
		ts(now()), reason, id)
	if err != nil {
		return fmt.Errorf("store: resolving an alert: %w", err)
	}
	return nil
}

// resolveAlertsFor closes every open alert of a monitor. Used when it is disabled or its rule
// changes underneath it.
func (s *Store) resolveAlertsFor(ctx context.Context, monitorID, reason string) error {
	_, err := s.exec(ctx, `UPDATE monitor_alerts
        SET state = 'resolved', resolved_at = ?, resolve_reason = ?
        WHERE monitor_id = ? AND state = 'firing'`,
		ts(now()), reason, monitorID)
	if err != nil {
		return fmt.Errorf("store: resolving a monitor's alerts: %w", err)
	}
	return nil
}

// StaleAlerts are firing alerts whose container has stopped producing samples — it was
// redeployed, or removed, or the host went away.
//
// They must be resolved. An alert that hangs firing forever on a container that no longer
// exists is worse than no alert at all: it is the one that teaches everybody to ignore the
// page, and then the next one goes unread with it.
func (s *Store) StaleAlerts(ctx context.Context, before time.Time) ([]*Alert, error) {
	rows, err := s.query(ctx, `SELECT id, monitor_id, env_id, container_name, container_id,
        stack, value FROM monitor_alerts
        WHERE state = 'firing' AND last_seen_at < ?`, ts(before))
	if err != nil {
		return nil, fmt.Errorf("store: finding stale alerts: %w", err)
	}
	defer rows.Close()

	var out []*Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.MonitorID, &a.EnvID, &a.ContainerName, &a.ContainerID,
			&a.Stack, &a.Value); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ListAlerts returns alerts a caller may see. It FILTERS, like every other list.
func (s *Store) ListAlerts(ctx context.Context, global bool, envs []string, limit int) ([]*Alert, error) {
	if limit <= 0 {
		limit = 100
	}

	q := `SELECT a.id, a.monitor_id, m.name, a.env_id, a.container_name, a.container_id,
             a.stack, a.state, a.value, a.started_at, a.last_seen_at, a.resolved_at,
             a.resolve_reason
          FROM monitor_alerts a
          JOIN resource_monitors m ON m.id = a.monitor_id`
	var args []any

	if !global {
		if len(envs) == 0 {
			return nil, nil
		}
		ph := make([]string, len(envs))
		for i, e := range envs {
			ph[i] = "?"
			args = append(args, e)
		}
		q += ` WHERE a.env_id IN (` + strings.Join(ph, ",") + `)`
	}

	// Firing first, then most recent. And the LIMIT is in the SQL, not applied afterwards in
	// Go — filtering a page after the database has already truncated it hands back a short
	// page and no indication that it did.
	q += ` ORDER BY CASE WHEN a.state = 'firing' THEN 0 ELSE 1 END, a.started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing alerts: %w", err)
	}
	defer rows.Close()

	var out []*Alert
	for rows.Next() {
		var (
			a                     Alert
			startedAt, lastSeenAt string
			resolvedAt            *string
		)
		if err := rows.Scan(&a.ID, &a.MonitorID, &a.MonitorName, &a.EnvID, &a.ContainerName,
			&a.ContainerID, &a.Stack, &a.State, &a.Value, &startedAt, &lastSeenAt,
			&resolvedAt, &a.ResolveReason); err != nil {
			return nil, err
		}
		a.StartedAt, a.LastSeenAt = parseTS(startedAt), parseTS(lastSeenAt)
		if resolvedAt != nil {
			t := parseTS(*resolvedAt)
			a.ResolvedAt = &t
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}
