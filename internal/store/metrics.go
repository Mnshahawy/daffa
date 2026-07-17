package store

// Storage for resource samples. See docs/monitoring.md.
//
// This is the one table in Daffa that is partitioned, and the one table whose timestamps are
// integers rather than RFC3339 text. Both are deliberate and both are explained below, because
// each of them looks like a mistake until you know why.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A Sample is one container, at one instant.
type Sample struct {
	TS            time.Time
	EnvID         string
	ContainerID   string
	ContainerName string
	Stack         string
	Service       string
	CPUPct        float64 // percent of the container's CPU allowance
	CPUCores      float64 // what that allowance is
	MemBytes      int64
	MemLimit      int64
	MemPct        float64
}

// metricCols is the column list, in the order Sample's fields are read and written. It is
// written once so a partition's DDL, the INSERT and the UNION-ALL read path cannot drift.
const metricCols = `ts_unix, env_id, container_id, container_name, stack, service,
    cpu_pct, cpu_cores, mem_bytes, mem_limit, mem_pct`

// day is the partition a timestamp belongs to: a UTC calendar day.
//
// The timestamps here are epoch SECONDS, not the RFC3339Nano text every other table in Daffa
// uses. That is not laziness, and it is the difference between this working and silently
// losing a day of data at every midnight.
//
// Go's RFC3339Nano trims trailing zeros from the fraction, so the text does not sort
// chronologically: "2026-07-14T00:00:00.5Z" < "2026-07-14T00:00:00Z", because '.' (0x2E) sorts
// before 'Z' (0x5A). A sample taken half a second after midnight would therefore compare LESS
// than its own day's partition boundary, land in yesterday's partition, and be dropped a day
// early. Nothing at runtime would ever tell you.
//
// Integers sort. They also make integer partition bounds, portable bucket arithmetic for
// downsampling, and cost 8 bytes instead of thirty.
func day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// partitionName is the child table for a day: metric_samples_20260713.
func partitionName(d time.Time) string {
	return "metric_samples_" + d.UTC().Format("20060102")
}

// ── the partitioned table ───────────────────────────────────────────────────────
//
// Postgres has declarative partitioning. SQLite has nothing at all. Both get daily CHILD
// tables with identical columns, so the INSERT is the same statement in both dialects;
// Postgres additionally gets a real parent for them to attach to, which gives it a queryable
// `metric_samples` and lets its planner do the pruning. On SQLite the reader does the pruning
// itself, in metricsSource, by unioning only the days it actually needs.
//
// Expiry is a DROP TABLE in both, which is the entire point of partitioning this: a DELETE of
// a day's rows is a large write transaction, and on SQLite it also leaves a file that never
// shrinks.

// InitMetrics creates the parent table (Postgres) and makes sure a read against an empty
// installation works. Called once at startup, after migrations.
func (s *Store) InitMetrics(ctx context.Context) error {
	if s.dialect != Postgres {
		// SQLite has no parent: `metric_samples` as a name exists only inside metricsSource,
		// which builds it out of whatever day tables are actually there. A fresh install has
		// none, and reads against none return no rows rather than failing — see metricsSource.
		return nil
	}

	_, err := s.exec(ctx, `CREATE TABLE IF NOT EXISTS metric_samples (
        ts_unix        BIGINT NOT NULL,
        env_id         TEXT NOT NULL,
        container_id   TEXT NOT NULL,
        container_name TEXT NOT NULL,
        stack          TEXT NOT NULL DEFAULT '',
        service        TEXT NOT NULL DEFAULT '',
        cpu_pct        DOUBLE PRECISION NOT NULL DEFAULT 0,
        cpu_cores      DOUBLE PRECISION NOT NULL DEFAULT 0,
        mem_bytes      BIGINT NOT NULL DEFAULT 0,
        mem_limit      BIGINT NOT NULL DEFAULT 0,
        mem_pct        DOUBLE PRECISION NOT NULL DEFAULT 0
    ) PARTITION BY RANGE (ts_unix)`)
	if err != nil {
		return fmt.Errorf("store: creating metric_samples: %w", err)
	}
	return nil
}

// EnsurePartition creates the day's child table if it is not there. Idempotent, and cheap
// enough to call before every write.
func (s *Store) EnsurePartition(ctx context.Context, t time.Time) error {
	d := day(t)
	name := partitionName(d)

	var stmt string
	if s.dialect == Postgres {
		stmt = fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF metric_samples FOR VALUES FROM (%d) TO (%d)`,
			name, d.Unix(), d.AddDate(0, 0, 1).Unix())
	} else {
		stmt = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
            ts_unix        INTEGER NOT NULL,
            env_id         TEXT NOT NULL,
            container_id   TEXT NOT NULL,
            container_name TEXT NOT NULL,
            stack          TEXT NOT NULL DEFAULT '',
            service        TEXT NOT NULL DEFAULT '',
            cpu_pct        REAL NOT NULL DEFAULT 0,
            cpu_cores      REAL NOT NULL DEFAULT 0,
            mem_bytes      INTEGER NOT NULL DEFAULT 0,
            mem_limit      INTEGER NOT NULL DEFAULT 0,
            mem_pct        REAL NOT NULL DEFAULT 0
        )`, name)
	}

	if _, err := s.exec(ctx, stmt); err != nil {
		return fmt.Errorf("store: creating partition %s: %w", name, err)
	}

	// The index goes on the child in both dialects. On Postgres an index on the parent would
	// propagate, but creating it here keeps the two paths saying the same thing in one place.
	//
	// (env_id, container_name, ts_unix) is what every read wants: the evaluator asks for one
	// container's window, the chart asks for one container's range. Never an index on ts_unix
	// alone — retention drops whole tables and has nothing to scan.
	_, err := s.exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_lookup_idx ON %s (env_id, container_name, ts_unix)`,
		name, name))
	if err != nil {
		return fmt.Errorf("store: indexing partition %s: %w", name, err)
	}
	return nil
}

// Partitions lists the day tables that exist, oldest first. Both dialects keep a catalogue we
// can ask, which is what makes retention idempotent: it works from what is actually on disk,
// not from what it believes it created.
func (s *Store) Partitions(ctx context.Context) ([]time.Time, error) {
	// The LIKE is a coarse filter — '_' is itself a wildcard in LIKE, so it matches a little
	// more than it means to. The date parse below is the real test, and anything whose suffix
	// is not a date is left alone. That matters: Daffa shares its Postgres schema with
	// whatever else the operator keeps there, and a retention sweep that guessed would be
	// dropping somebody else's tables.
	q := `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'metric_samples_%' ORDER BY name`
	args := []any{}
	if s.dialect == Postgres {
		q = `SELECT tablename FROM pg_tables WHERE schemaname = ? AND tablename LIKE 'metric_samples_%' ORDER BY tablename`
		args = []any{s.pgSchema}
	}

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing partitions: %w", err)
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		d, err := time.ParseInLocation("20060102", strings.TrimPrefix(name, "metric_samples_"), time.UTC)
		if err != nil {
			continue // not one of ours; leave it alone
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DropPartitionsBefore expires old samples by DROPPING the day tables, not by deleting rows.
// It returns the days it dropped.
func (s *Store) DropPartitionsBefore(ctx context.Context, cutoff time.Time) ([]time.Time, error) {
	parts, err := s.Partitions(ctx)
	if err != nil {
		return nil, err
	}

	cut := day(cutoff)
	var dropped []time.Time
	for _, d := range parts {
		if !d.Before(cut) {
			continue
		}
		if _, err := s.exec(ctx, "DROP TABLE IF EXISTS "+partitionName(d)); err != nil {
			return dropped, fmt.Errorf("store: dropping partition %s: %w", partitionName(d), err)
		}
		dropped = append(dropped, d)
	}
	return dropped, nil
}

// ── writing ─────────────────────────────────────────────────────────────────────

// InsertSamples writes one collection round.
//
// Every sample in a round shares the round's timestamp, so they all belong to one partition —
// which is why this is a single multi-row INSERT into a named child table, and why the
// statement is identical on both dialects (a Postgres partition is directly insertable, and
// deriving the name from the row's own timestamp means it cannot be the wrong one).
func (s *Store) InsertSamples(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	if err := s.EnsurePartition(ctx, samples[0].TS); err != nil {
		return err
	}
	table := partitionName(day(samples[0].TS))

	// Chunked: a host with hundreds of containers must not build one statement with thousands
	// of placeholders, which both drivers will refuse somewhere north of a few thousand.
	const chunk = 200
	for start := 0; start < len(samples); start += chunk {
		end := min(start+chunk, len(samples))

		var (
			ph   = make([]string, 0, end-start)
			args = make([]any, 0, (end-start)*11)
		)
		for _, m := range samples[start:end] {
			ph = append(ph, "(?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, m.TS.UTC().Unix(), m.EnvID, m.ContainerID, m.ContainerName,
				m.Stack, m.Service, m.CPUPct, m.CPUCores, m.MemBytes, m.MemLimit, m.MemPct)
		}

		_, err := s.exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES %s",
			table, metricCols, strings.Join(ph, ",")), args...)
		if err != nil {
			return fmt.Errorf("store: inserting %d samples: %w", end-start, err)
		}
	}
	return nil
}

// ── reading ─────────────────────────────────────────────────────────────────────

// metricsSource returns the FROM expression covering [from, to], and the args it needs.
//
// This is the ONLY place either dialect's partitioning shows through. Postgres reads the
// parent and lets its planner prune. SQLite has no parent, so we do the pruning: a UNION ALL
// over exactly the day tables that overlap the range — which for a one-hour chart is one
// table, not seven days of them.
//
// A range with no partitions behind it (a fresh install, or a window older than retention)
// yields a source that returns no rows rather than an error. Every caller would otherwise
// carry the same special case, and one of them would eventually get it wrong.
// The `AS ms` alias is not decoration: Postgres REQUIRES a subquery in FROM to be aliased,
// and SQLite does not care, so aliasing always is the one form that is valid in both.
func (s *Store) metricsSource(ctx context.Context, from, to time.Time) (string, error) {
	if s.dialect == Postgres {
		return "metric_samples AS ms", nil
	}

	have, err := s.Partitions(ctx)
	if err != nil {
		return "", err
	}
	want := map[time.Time]bool{}
	for d := day(from); !d.After(day(to)); d = d.AddDate(0, 0, 1) {
		want[d] = true
	}

	var parts []string
	for _, d := range have {
		if want[d] {
			parts = append(parts, "SELECT "+metricCols+" FROM "+partitionName(d))
		}
	}
	if len(parts) == 0 {
		// The right shape, and empty. `WHERE 0` is never true, so this returns no rows while
		// the caller's WHERE and GROUP BY still parse against real column names.
		return "(SELECT 0 AS ts_unix, '' AS env_id, '' AS container_id, '' AS container_name," +
			" '' AS stack, '' AS service, 0 AS cpu_pct, 0 AS cpu_cores, 0 AS mem_bytes," +
			" 0 AS mem_limit, 0 AS mem_pct WHERE 0) AS ms", nil
	}
	return "(" + strings.Join(parts, " UNION ALL ") + ") AS ms", nil
}

// Point is one bucket of a chart series.
type Point struct {
	TS       time.Time `json:"ts"`
	CPUAvg   float64   `json:"cpu_avg"`
	CPUMax   float64   `json:"cpu_max"`
	MemAvg   float64   `json:"mem_avg"` // bytes
	MemMax   float64   `json:"mem_max"`
	MemPct   float64   `json:"mem_pct"`
	MemLimit float64   `json:"mem_limit"`
}

// SeriesQuery asks for one chart.
//
// Container and Stack are optional filters. With neither, the series is the whole host: every
// container on it, summed.
type SeriesQuery struct {
	EnvID     string
	Container string // container_name
	Stack     string
	From, To  time.Time
	// MaxPoints caps what crosses the wire. Seven days of 30-second samples is 20,000 points
	// per container; sending them all to draw 600 pixels is a megabyte of JSON and a browser
	// doing the downsampling we could have done in SQL.
	MaxPoints int
}

// Series returns bucketed CPU and memory over a range.
//
// Buckets carry an average AND a maximum. Only the mean would smooth away the spike that
// somebody opened the chart to find, which rather defeats the purpose of having kept it.
//
// Note the two-level aggregation when no single container is named: within a bucket, the
// containers on the host at that instant must be SUMMED (a stack using 40% is its containers'
// usage added up) and only then averaged across the instants in the bucket. Averaging first
// would report a stack's usage as its *mean container's* usage, which is a number with no
// meaning at all.
func (s *Store) Series(ctx context.Context, q SeriesQuery) ([]Point, error) {
	if q.MaxPoints <= 0 {
		q.MaxPoints = 240
	}

	src, err := s.metricsSource(ctx, q.From, q.To)
	if err != nil {
		return nil, err
	}

	bucket := int64(q.To.Sub(q.From).Seconds()) / int64(q.MaxPoints)
	if bucket < 1 {
		bucket = 1
	}

	where := []string{"ts_unix >= ?", "ts_unix < ?", "env_id = ?"}
	args := []any{q.From.UTC().Unix(), q.To.UTC().Unix(), q.EnvID}
	if q.Container != "" {
		where = append(where, "container_name = ?")
		args = append(args, q.Container)
	}
	if q.Stack != "" {
		where = append(where, "stack = ?")
		args = append(args, q.Stack)
	}

	// Inner: collapse each instant to one row (summing the containers in scope at that
	// instant). Outer: average and peak those instants within the bucket.
	//
	// Integer division does the bucketing, and it is integer division in BOTH dialects
	// because ts_unix and the bucket width are both integers — which is the third quiet
	// dividend of not storing these timestamps as text.
	query := fmt.Sprintf(`
        SELECT b, AVG(cpu), MAX(cpu), AVG(mem), MAX(mem), MAX(pct), MAX(lim)
        FROM (
            SELECT (ts_unix / %d) * %d AS b,
                   SUM(cpu_pct)   AS cpu,
                   SUM(mem_bytes) AS mem,
                   MAX(mem_pct)   AS pct,
                   SUM(mem_limit) AS lim
            FROM %s
            WHERE %s
            GROUP BY ts_unix
        ) t
        GROUP BY b
        ORDER BY b`,
		bucket, bucket, src, strings.Join(where, " AND "))

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading series: %w", err)
	}
	defer rows.Close()

	var out []Point
	for rows.Next() {
		var (
			b int64
			p Point
		)
		if err := rows.Scan(&b, &p.CPUAvg, &p.CPUMax, &p.MemAvg, &p.MemMax, &p.MemPct, &p.MemLimit); err != nil {
			return nil, err
		}
		p.TS = time.Unix(b, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// MetricsUsage is what the settings page shows: a feature that quietly grows a database owes
// the person paying for the disk an honest number.
type MetricsUsage struct {
	Samples    int64      `json:"samples"`
	Partitions int        `json:"partitions"`
	Oldest     *time.Time `json:"oldest"`
	Bytes      int64      `json:"bytes"` // estimated
}

// Usage counts what has accumulated.
func (s *Store) Usage(ctx context.Context) (*MetricsUsage, error) {
	parts, err := s.Partitions(ctx)
	if err != nil {
		return nil, err
	}
	u := &MetricsUsage{Partitions: len(parts)}
	if len(parts) == 0 {
		return u, nil
	}

	// Count across the whole retained range, not per partition: one query either way, and on
	// Postgres the parent already spans them.
	src, err := s.metricsSource(ctx, parts[0], parts[len(parts)-1])
	if err != nil {
		return nil, err
	}

	var oldest *int64
	if err := s.queryRow(ctx,
		"SELECT COUNT(*), MIN(ts_unix) FROM "+src).Scan(&u.Samples, &oldest); err != nil {
		return nil, fmt.Errorf("store: counting samples: %w", err)
	}
	if oldest != nil {
		t := time.Unix(*oldest, 0).UTC()
		u.Oldest = &t
	}

	// Estimated, and labelled as such wherever it is shown. The exact figure differs by dialect,
	// by page fill and by index overhead; the useful question is "is this about to eat my disk",
	// and about 90 bytes a row answers it.
	u.Bytes = u.Samples * 90
	return u, nil
}

// MetricsSourceForTest exposes the partition-spanning FROM expression to the monitor package's
// live test, which reads samples back raw to check them against a real daemon.
func (s *Store) MetricsSourceForTest(ctx context.Context, from, to time.Time) (string, error) {
	return s.metricsSource(ctx, from, to)
}
