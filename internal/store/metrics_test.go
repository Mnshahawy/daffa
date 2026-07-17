package store

import (
	"context"
	"testing"
	"time"
)

// THE test for this table.
//
// Daffa stores every other timestamp as RFC3339Nano text, and Go trims trailing zeros from the
// fraction — so "2026-07-14T00:00:00.5Z" sorts BEFORE "2026-07-14T00:00:00Z", because '.' is
// 0x2E and 'Z' is 0x5A. Had the partition key been that text, a sample taken half a second
// after midnight would compare less than its own day's lower bound, land in yesterday's
// partition, and be dropped a day early. Nothing at runtime would ever have said so.
//
// This test writes samples either side of a midnight, and insists each one is readable in the
// window it actually belongs to.
func TestSamplesLandInTheRightDay(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		midnight := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
		yesterday, today := midnight.AddDate(0, 0, -1), midnight

		// The last moment of the 13th, and half a second into the 14th. That second one is the
		// whole test: under an RFC3339 text key it renders as "2026-07-14T00:00:00.5Z", which
		// compares LESS than the 14th's lower bound of "2026-07-14T00:00:00Z", and so files
		// itself under the 13th and dies a day early.
		for _, at := range []time.Time{
			midnight.Add(-time.Second),
			midnight.Add(500 * time.Millisecond),
		} {
			if err := s.InsertSamples(ctx, []Sample{{
				TS: at, EnvID: env, ContainerID: "abc", ContainerName: "api",
				CPUPct: 50, MemBytes: 100, MemLimit: 200, MemPct: 50,
			}}); err != nil {
				t.Fatalf("inserting a sample at %s: %v", at.Format(time.RFC3339Nano), err)
			}
		}

		// Assert on the PARTITIONS, not on a query result. A range query would pass even if
		// both rows were in one table; the question here is which table each row went into.
		if n := rowsIn(t, s, yesterday); n != 1 {
			t.Errorf("the 13th's partition holds %d rows; want 1 (the 23:59:59 sample)", n)
		}
		if n := rowsIn(t, s, today); n != 1 {
			t.Errorf("the 14th's partition holds %d rows; want 1. The sample at 00:00:00.5 "+
				"has filed itself under the PREVIOUS day, where retention will drop it a day "+
				"early — which is exactly what an RFC3339-text partition key does, and why "+
				"this column is an integer.", n)
		}

		// And one query spanning the midnight sees both — which on SQLite means the UNION in
		// metricsSource really did reach across two day tables, rather than quietly reading
		// only the one the range started in.
		if n := countSamples(t, s, yesterday, midnight.AddDate(0, 0, 1)); n != 2 {
			t.Errorf("a query spanning the midnight saw %d samples; want 2 — the read is not "+
				"reaching across the day boundary", n)
		}
	})
}

// Retention DROPS the day, it does not DELETE the rows. That is the entire point of
// partitioning this table, so it deserves an assertion about the tables and not merely about
// the row count: a DELETE would pass a row-count check while leaving the large write
// transaction, and the SQLite file that never shrinks, exactly where they were.
func TestRetentionDropsPartitions(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		now := time.Now().UTC()
		for _, ago := range []int{0, 1, 5, 9, 20} {
			at := now.AddDate(0, 0, -ago)
			if err := s.InsertSamples(ctx, []Sample{{
				TS: at, EnvID: env, ContainerID: "abc", ContainerName: "api", CPUPct: 1,
			}}); err != nil {
				t.Fatal(err)
			}
		}

		before, err := s.Partitions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(before) != 5 {
			t.Fatalf("wrote five days and got %d partitions — one day is meant to be one table", len(before))
		}

		// A seven-day retention keeps today, yesterday and the five-day-old one.
		dropped, err := s.DropPartitionsBefore(ctx, now.AddDate(0, 0, -7))
		if err != nil {
			t.Fatal(err)
		}
		if len(dropped) != 2 {
			t.Errorf("dropped %d partitions; want 2 (the 9- and 20-day-old ones)", len(dropped))
		}

		after, err := s.Partitions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != 3 {
			t.Errorf("%d partitions survived a 7-day retention; want 3", len(after))
		}
		for _, d := range after {
			if d.Before(day(now.AddDate(0, 0, -7))) {
				t.Errorf("a partition older than the cutoff survived: %s", d.Format(time.DateOnly))
			}
		}
	})
}

// A fresh install has no partitions at all. Reading from it must return nothing — not an
// error, and certainly not "no such table". Every caller would otherwise carry the same
// special case, and one of them would get it wrong.
func TestReadingAnEmptyStoreIsNotAnError(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		pts, err := s.Series(ctx, SeriesQuery{
			EnvID: "env_nothing",
			From:  time.Now().Add(-time.Hour),
			To:    time.Now(),
		})
		if err != nil {
			t.Fatalf("reading a store with no samples in it: %v", err)
		}
		if len(pts) != 0 {
			t.Errorf("an empty store returned %d points", len(pts))
		}

		u, err := s.Usage(ctx)
		if err != nil {
			t.Fatalf("usage on an empty store: %v", err)
		}
		if u.Samples != 0 || u.Partitions != 0 || u.Oldest != nil {
			t.Errorf("an empty store reports %+v", u)
		}
	})
}

// Buckets carry an average AND a maximum, and the reason is this: a spike is the thing you
// opened the chart to find, and a mean is precisely the operation that hides it.
func TestBucketsKeepThePeak(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		base := time.Now().UTC().Truncate(time.Hour)

		// Ten minutes of an idle container, with one minute of it pegged.
		for i := range 20 {
			cpu := 5.0
			if i == 10 {
				cpu = 95.0
			}
			if err := s.InsertSamples(ctx, []Sample{{
				TS: base.Add(time.Duration(i) * 30 * time.Second), EnvID: env,
				ContainerID: "abc", ContainerName: "api", CPUPct: cpu,
				MemBytes: 1000, MemLimit: 2000, MemPct: 50,
			}}); err != nil {
				t.Fatal(err)
			}
		}

		// One bucket over the whole ten minutes.
		pts, err := s.Series(ctx, SeriesQuery{
			EnvID: env, Container: "api",
			From: base, To: base.Add(10 * time.Minute), MaxPoints: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(pts) != 1 {
			t.Fatalf("got %d buckets; want 1", len(pts))
		}

		if pts[0].CPUMax < 94 {
			t.Errorf("the peak was averaged away: max = %.1f, want ~95. A chart that "+
				"smooths out the spike is a chart that answers no question anyone had.", pts[0].CPUMax)
		}
		if pts[0].CPUAvg > 20 {
			t.Errorf("the average is %.1f; want ~9.5 — one spike in twenty samples", pts[0].CPUAvg)
		}
	})
}

// A stack's usage is its containers ADDED UP at each instant, and only then averaged over the
// bucket. Averaging first would report a stack's CPU as its *mean container's* CPU — a number
// that means nothing, and one that would quietly make a busy stack look idle simply by adding
// a sidecar to it.
func TestAStacksUsageIsTheSumOfItsContainers(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		env := oneHost(t, s)

		at := time.Now().UTC().Truncate(time.Minute)
		if err := s.InsertSamples(ctx, []Sample{
			{TS: at, EnvID: env, ContainerName: "billing-api-1", Stack: "billing", CPUPct: 30, MemBytes: 100},
			{TS: at, EnvID: env, ContainerName: "billing-worker-1", Stack: "billing", CPUPct: 40, MemBytes: 200},
			// A container in ANOTHER stack, at the same instant, which must not be counted.
			{TS: at, EnvID: env, ContainerName: "web-1", Stack: "web", CPUPct: 90, MemBytes: 900},
		}); err != nil {
			t.Fatal(err)
		}

		pts, err := s.Series(ctx, SeriesQuery{
			EnvID: env, Stack: "billing",
			From: at.Add(-time.Minute), To: at.Add(time.Minute), MaxPoints: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(pts) != 1 {
			t.Fatalf("got %d buckets; want 1", len(pts))
		}
		if pts[0].CPUAvg < 69 || pts[0].CPUAvg > 71 {
			t.Errorf("the stack's CPU is %.1f; want 70 (30 + 40). If it is 35, the containers "+
				"were averaged instead of summed; if it is 160, the other stack leaked in.",
				pts[0].CPUAvg)
		}
		if pts[0].MemAvg < 299 || pts[0].MemAvg > 301 {
			t.Errorf("the stack's memory is %.0f; want 300", pts[0].MemAvg)
		}
	})
}

// Retention has a ceiling, and it is enforced with a sentence rather than a constraint
// violation, because "90" is a number somebody has to be told the reason for.
func TestRetentionIsCapped(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		err := s.SaveMonitorSettings(ctx, &MonitorSettings{
			Enabled: true, IntervalSecs: 30, RetentionDays: 365,
		})
		if err == nil {
			t.Fatal("a year of retention was accepted — on SQLite that is a 365-way UNION on " +
				"every read, and past SQLITE_MAX_COMPOUND_SELECT it simply stops working")
		}

		if err := s.SaveMonitorSettings(ctx, &MonitorSettings{
			Enabled: true, IntervalSecs: 30, RetentionDays: MaxRetentionDays,
		}); err != nil {
			t.Errorf("the maximum retention was refused: %v", err)
		}

		// And an interval nobody meant to type.
		if err := s.SaveMonitorSettings(ctx, &MonitorSettings{
			Enabled: true, IntervalSecs: 1, RetentionDays: 7,
		}); err == nil {
			t.Error("a one-second sampling interval was accepted")
		}
	})
}

func TestMonitorSettingsDefaultsAreSane(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		m, err := s.MonitorSettings(context.Background())
		if err != nil {
			t.Fatalf("reading unconfigured monitor settings: %v", err)
		}
		if !m.Enabled || m.IntervalSecs != 30 || m.RetentionDays != 7 {
			t.Errorf("a fresh install defaults to %+v; want enabled, 30s, 7 days", m)
		}
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────────

func oneHost(t *testing.T, s *Store) string {
	t.Helper()
	e := &Environment{Name: "host"}
	if err := s.CreateEnvironment(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// rowsIn counts what is physically in one day's partition table — the only way to ask "which
// table did this row go into", which is the question the boundary test is actually asking.
func rowsIn(t *testing.T, s *Store, d time.Time) int {
	t.Helper()
	var n int
	if err := s.queryRow(context.Background(),
		"SELECT COUNT(*) FROM "+partitionName(day(d))).Scan(&n); err != nil {
		t.Fatalf("counting rows in %s: %v", partitionName(day(d)), err)
	}
	return n
}

// countSamples counts rows through metricsSource — i.e. through the same partition-spanning
// read path the real queries use, rather than through one table.
func countSamples(t *testing.T, s *Store, from, to time.Time) int {
	t.Helper()
	ctx := context.Background()

	src, err := s.metricsSource(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.queryRow(ctx,
		"SELECT COUNT(*) FROM "+src+" WHERE ts_unix >= ? AND ts_unix < ?",
		from.UTC().Unix(), to.UTC().Unix()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seriesLen(t *testing.T, s *Store, env string, from, to time.Time) int {
	t.Helper()
	pts, err := s.Series(context.Background(), SeriesQuery{
		EnvID: env, From: from, To: to, MaxPoints: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(pts)
}
