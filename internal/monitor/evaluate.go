package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// Evaluator decides what is firing.
//
// It runs immediately after each collection round rather than on a ticker of its own, so it
// always sees the sample that was just written. Two independent tickers would race, and the
// loser would be evaluating a window that is one interval stale — which for a rule with a
// coverage floor means it would intermittently decide there was not enough data.
type Evaluator struct {
	store  *store.Store
	notify *notify.Notifier
	log    *slog.Logger
}

func NewEvaluator(st *store.Store, n *notify.Notifier, log *slog.Logger) *Evaluator {
	return &Evaluator{store: st, notify: n, log: log}
}

// Run evaluates every enabled monitor against the round that just landed.
func (e *Evaluator) Run(ctx context.Context, at time.Time) {
	cfg, err := e.store.MonitorSettings(ctx)
	if err != nil {
		e.log.Error("monitor: reading settings", "err", err)
		return
	}

	monitors, err := e.store.EnabledMonitors(ctx)
	if err != nil {
		e.log.Error("monitor: listing monitors", "err", err)
		return
	}

	for _, m := range monitors {
		e.evaluate(ctx, m, at, cfg.Interval())
	}

	e.resolveStale(ctx, at, cfg.Interval())
}

func (e *Evaluator) evaluate(ctx context.Context, m *store.Monitor, at time.Time, interval time.Duration) {
	breaches, err := e.store.Evaluate(ctx, m, at)
	if err != nil {
		e.log.Error("monitor: evaluating", "monitor", m.Name, "err", err)
		return
	}

	// How many samples the window SHOULD hold, which is what the coverage floor is measured
	// against. Derived from the interval rather than assumed, so shortening the interval does
	// not silently make every rule harder to satisfy.
	expected := int(time.Duration(m.DurationSecs) * time.Second / interval)

	// Newly-firing alerts, grouped by host — see the note on notifyFired for why the grouping
	// is by HOST and not merely by monitor.
	fired := map[string][]*store.Alert{}

	for _, b := range breaches {
		open, err := e.store.FiringAlert(ctx, m.ID, b.ContainerName)
		firing := err == nil

		// Not enough samples to say anything. Say nothing.
		//
		// This is NOT the same as recovery, and reading it as recovery is a bug I shipped and
		// then watched happen against a real daemon: I stopped the container an alert was about,
		// its samples aged out of the window, coverage collapsed, "sustained" went false, and
		// the alert resolved itself with the words "back within the threshold (100%)" — a
		// sentence that contradicts itself, about a container that no longer existed.
		//
		// Absence of evidence is not evidence of recovery. Leaving the alert untouched lets its
		// last_seen_at go stale, and resolveStale then closes it for the reason it actually
		// happened: the container stopped reporting.
		if !b.HasCoverage(expected) {
			continue
		}

		switch {
		case b.Sustained(expected) && !firing:
			a := &store.Alert{
				MonitorID: m.ID, EnvID: b.EnvID, ContainerName: b.ContainerName,
				ContainerID: b.ContainerID, Stack: b.Stack, Value: b.Worst,
			}
			if err := e.store.RaiseAlert(ctx, a); err != nil {
				e.log.Error("monitor: raising an alert", "monitor", m.Name, "err", err)
				continue
			}
			fired[b.EnvID] = append(fired[b.EnvID], a)

		case b.Sustained(expected):
			// Still bad. Keep the value current so the UI shows what it is doing now, and do
			// NOT notify again — a monitor that re-pages every thirty seconds is a monitor
			// that gets muted, and then it is no longer a monitor.
			if err := e.store.TouchAlert(ctx, open.ID, b.Worst); err != nil {
				e.log.Error("monitor: touching an alert", "err", err)
			}

		case firing:
			// Genuinely recovered: the window has enough samples AND at least one of them
			// stopped breaching, so the run is broken and the clock resets.
			//
			// Report Best, not Worst — the value it came back to, not the worst it ever was.
			// "Back within the threshold (100%)" is how you lose an operator's trust in every
			// other number on the page.
			e.resolve(ctx, m, open, fmt.Sprintf("back within the threshold (%s)",
				format(m.Metric, b.Best)))
		}
	}

	for envID, alerts := range fired {
		e.notifyFired(ctx, m, envID, alerts)
	}
}

// resolveStale closes alerts whose container has gone quiet.
//
// Three intervals of silence means the container was redeployed, removed, or its host went
// away. Whatever the reason, the alert is about something that no longer exists — and an alert
// that hangs firing forever is the one that teaches people to ignore the page.
func (e *Evaluator) resolveStale(ctx context.Context, at time.Time, interval time.Duration) {
	stale, err := e.store.StaleAlerts(ctx, at.Add(-3*interval))
	if err != nil {
		e.log.Error("monitor: finding stale alerts", "err", err)
		return
	}

	for _, a := range stale {
		m, err := e.store.MonitorByID(ctx, a.MonitorID)
		if err != nil {
			continue
		}
		e.resolve(ctx, m, a, "the container stopped reporting — it was redeployed, removed, "+
			"or its host went away")
	}
}

func (e *Evaluator) resolve(ctx context.Context, m *store.Monitor, a *store.Alert, reason string) {
	if err := e.store.ResolveAlert(ctx, a.ID, reason); err != nil {
		e.log.Error("monitor: resolving an alert", "err", err)
		return
	}

	e.log.Info("monitor: recovered", "monitor", m.Name, "container", a.ContainerName)

	e.notify.Send(ctx, a.EnvID, notify.Data{
		Event:   notify.MonitorResolved,
		Subject: fmt.Sprintf("Recovered: %s — %s", m.Name, a.ContainerName),
		Title:   fmt.Sprintf("%s recovered", a.ContainerName),
		Summary: fmt.Sprintf("%q is no longer firing. It had been since %s.",
			m.Name, a.StartedAt.Format(time.RFC1123)),
		Target: a.ContainerName,
		Detail: reason,
		Link:   "/monitors",
	})
}

// notifyFired sends ONE message for everything a monitor just tripped on a host.
//
// A monitor matching a hundred containers on a host that has just run out of memory would
// otherwise send a hundred emails — and the hundred-email version of an alert is functionally
// identical to no alert, because nobody reads any of them.
//
// The grouping is per HOST, not merely per monitor, and that is not tidiness: recipients are
// resolved per host (RecipientsFor takes the env), so one message spanning two hosts would
// have to go to the union of their recipients — which is exactly how a staging-scoped operator
// ends up being paged about production. See docs/scoping.md.
func (e *Evaluator) notifyFired(ctx context.Context, m *store.Monitor, envID string, alerts []*store.Alert) {
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].ContainerName < alerts[j].ContainerName })

	rule := fmt.Sprintf("%s %s %s for %s",
		metricName(m.Metric), m.Op, format(m.Metric, m.Threshold), humanDuration(m.DurationSecs))

	var (
		subject string
		title   string
		detail  strings.Builder
	)

	if len(alerts) == 1 {
		a := alerts[0]
		subject = fmt.Sprintf("%s — %s", m.Name, a.ContainerName)
		title = fmt.Sprintf("%s: %s is at %s", m.Name, a.ContainerName, format(m.Metric, a.Value))
	} else {
		subject = fmt.Sprintf("%s — %d containers", m.Name, len(alerts))
		title = fmt.Sprintf("%s: %d containers", m.Name, len(alerts))
	}

	for _, a := range alerts {
		fmt.Fprintf(&detail, "%-40s %s", a.ContainerName, format(m.Metric, a.Value))
		if a.Stack != "" {
			fmt.Fprintf(&detail, "   (stack: %s)", a.Stack)
		}
		detail.WriteString("\n")
	}

	e.log.Warn("monitor: firing", "monitor", m.Name, "containers", len(alerts), "env", envID)

	e.notify.Send(ctx, envID, notify.Data{
		Event:   notify.MonitorFired,
		Subject: subject,
		Title:   title,
		Summary: fmt.Sprintf("The rule is: %s.", rule),
		Target:  m.Name,
		Detail:  strings.TrimRight(detail.String(), "\n"),
		Link:    "/monitors",
		Failed:  true,
	})
}

// ── how a rule reads to a person ────────────────────────────────────────────────

func metricName(metric string) string {
	switch metric {
	case "cpu_pct", "cpu_cores":
		return "CPU"
	case "mem_pct", "mem_bytes":
		return "memory"
	}
	return metric
}

// format renders a value in the unit its metric is actually in. A memory alert that says
// "1073741824" instead of "1.0 GB" is an alert somebody has to do arithmetic on at 3am — and a
// CPU alert that says "1.5" without saying "vCPU" is one they have to guess at, because the
// other CPU rule on the same page is measured in percent.
func format(metric string, v float64) string {
	switch metric {
	case "mem_bytes":
		return humanBytes(v)
	case "cpu_cores":
		return humanCores(v)
	}
	return fmt.Sprintf("%.0f%%", v)
}

func humanBytes(v float64) string {
	const unit = 1024.0
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", v)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// humanCores drops the trailing zeros a fixed precision would leave behind: two cores is
// "2 vCPU", not "2.00 vCPU", and half a core is still "0.5 vCPU".
func humanCores(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64) + " vCPU"
}

func humanDuration(secs int) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", secs)
}
