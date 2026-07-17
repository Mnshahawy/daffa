package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/Mnshahawy/daffa/internal/store"
)

// Retention keeps the sample table's partitions in shape: tomorrow's exists before it is
// needed, and everything past its expiry is gone.
type Retention struct {
	store *store.Store
	log   *slog.Logger
}

func NewRetention(st *store.Store, log *slog.Logger) *Retention {
	return &Retention{store: st, log: log}
}

// Run sweeps at startup and hourly after that.
//
// Hourly, not daily. A daily sweep has to decide WHEN, and whenever it decides, that is the one
// moment a restart can skip — so a box that reboots at 03:00 every night would never create a
// partition, and every write would fail until somebody noticed. Hourly makes the sweep dull,
// and a dull sweep is one you never think about again.
func (r *Retention) Run(ctx context.Context) {
	r.Sweep(ctx)

	t := time.NewTicker(time.Hour)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

func (r *Retention) Sweep(ctx context.Context) {
	cfg, err := r.store.MonitorSettings(ctx)
	if err != nil {
		r.log.Error("monitor: reading settings for the retention sweep", "err", err)
		return
	}

	now := time.Now().UTC()

	// Today's, and TOMORROW'S. Creating tomorrow's a day early is the whole reason the midnight
	// roll is uneventful: the alternative is that the first write after 00:00 is also the write
	// that has to create the table it is going into, and that is a race — with the sweep, and
	// with every other writer — decided at the worst possible moment.
	//
	// Note this happens even when sampling is DISABLED. Someone switching it back on should
	// find a table waiting, not a first round that fails.
	for _, d := range []time.Time{now, now.AddDate(0, 0, 1)} {
		if err := r.store.EnsurePartition(ctx, d); err != nil {
			r.log.Error("monitor: creating a partition", "day", d.Format(time.DateOnly), "err", err)
			return
		}
	}

	// Expiry is a DROP, not a DELETE. That is the point of partitioning this table: deleting a
	// day of rows is a large write transaction competing with the sampler that is still
	// writing, and on SQLite it leaves a file that never gives the space back.
	cutoff := now.AddDate(0, 0, -cfg.RetentionDays)
	dropped, err := r.store.DropPartitionsBefore(ctx, cutoff)
	if err != nil {
		r.log.Error("monitor: dropping expired partitions", "err", err)
		return
	}
	if len(dropped) > 0 {
		days := make([]string, 0, len(dropped))
		for _, d := range dropped {
			days = append(days, d.Format(time.DateOnly))
		}
		r.log.Info("monitor: expired samples", "days", days, "retention_days", cfg.RetentionDays)
	}
}
