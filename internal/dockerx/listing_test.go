package dockerx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFetch is a daemon whose listing takes `took` and counts how often, and how concurrently, it
// was asked.
type fakeFetch struct {
	took            time.Duration
	calls, inFlight atomic.Int32
	maxInFlight     atomic.Int32
	fail            atomic.Bool
	sawCancelledCtx atomic.Bool
}

func (f *fakeFetch) fetch(ctx context.Context, all bool) ([]Container, error) {
	f.calls.Add(1)
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxInFlight.Load()
		if n <= m || f.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-time.After(f.took):
	case <-ctx.Done():
		f.sawCancelledCtx.Store(true)
		return nil, ctx.Err()
	}
	if f.fail.Load() {
		return nil, errors.New("daemon wedged")
	}
	return []Container{{ID: "a", Name: "web"}}, nil
}

func newTestLister() *containerLister {
	return &containerLister{gap: 20 * time.Millisecond, timeout: time.Second}
}

func TestListerSharesOneListingAcrossConcurrentCallers(t *testing.T) {
	l, f := newTestLister(), &fakeFetch{took: 50 * time.Millisecond}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.get(context.Background(), true, f.fetch); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	// Fifty callers arriving together: the first starts a listing, the rest either join it before
	// it starts or share the one queued behind it. Never fifty.
	if got := f.calls.Load(); got > 2 {
		t.Errorf("50 concurrent callers cost %d daemon listings, want at most 2", got)
	}
	if got := f.maxInFlight.Load(); got != 1 {
		t.Errorf("%d listings ran at once on one node, want 1", got)
	}
}

func TestListerNeverServesAListingThatStartedBeforeTheCall(t *testing.T) {
	l, f := newTestLister(), &fakeFetch{took: 60 * time.Millisecond}

	first := make(chan struct{})
	go func() {
		_, _ = l.get(context.Background(), true, f.fetch)
		close(first)
	}()
	// Wait until the first listing is under way, then ask: it may predate whatever this caller
	// is asking about, so the caller must get a NEW listing, not that one.
	for f.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if _, err := l.get(context.Background(), true, f.fetch); err != nil {
		t.Fatal(err)
	}
	<-first
	if got := f.calls.Load(); got != 2 {
		t.Errorf("a caller arriving mid-listing was answered by %d listings, want 2 (it waits for the next)", got)
	}
}

func TestListerCallerCancelDoesNotCancelTheSharedListing(t *testing.T) {
	l, f := newTestLister(), &fakeFetch{took: 80 * time.Millisecond}

	quitter, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := l.get(quitter, true, f.fetch)
		done <- err
	}()
	for f.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	// A second caller is waiting for the same (queued) listing behind it; the first walks away.
	stayer := make(chan error, 1)
	go func() {
		_, err := l.get(context.Background(), true, f.fetch)
		stayer <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that left got %v, want context.Canceled", err)
	}
	if err := <-stayer; err != nil {
		t.Fatalf("the caller that stayed got %v", err)
	}
	if f.sawCancelledCtx.Load() {
		t.Error("a caller leaving cancelled the daemon listing itself")
	}
}

func TestListerSkipsAListingNobodyWaitsFor(t *testing.T) {
	l, f := newTestLister(), &fakeFetch{took: 50 * time.Millisecond}

	go func() { _, _ = l.get(context.Background(), true, f.fetch) }()
	for f.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// Queue a second listing, then abandon it before it can start.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, _ = l.get(ctx, true, f.fetch)

	time.Sleep(150 * time.Millisecond)
	if got := f.calls.Load(); got != 1 {
		t.Errorf("an abandoned queued listing still ran: %d listings, want 1", got)
	}
}

func TestListerKeepsAndHandsOutCopiesOfTheLastGoodListing(t *testing.T) {
	l, f := newTestLister(), &fakeFetch{took: time.Millisecond}

	if _, _, ok := l.snapshot(true); ok {
		t.Fatal("a snapshot before any listing")
	}
	list, err := l.get(context.Background(), true, f.fetch)
	if err != nil {
		t.Fatal(err)
	}
	// Callers tag rows in place (fan-out sets Node). That must not reach anyone else's copy.
	list[0].Node = "mutated"

	f.fail.Store(true)
	if _, err := l.get(context.Background(), true, f.fetch); err == nil {
		t.Fatal("a failing daemon returned no error")
	}
	snap, at, ok := l.snapshot(true)
	if !ok || time.Since(at) > time.Second {
		t.Fatalf("no recent snapshot after a good listing (ok=%v at=%v)", ok, at)
	}
	if len(snap) != 1 || snap[0].ID != "a" || snap[0].Node != "" {
		t.Errorf("snapshot = %+v; a caller's edit leaked into the shared listing", snap)
	}
	// The running-only slot is separate.
	if _, _, ok := l.snapshot(false); ok {
		t.Error("an all=true listing answered for all=false")
	}
}

func TestListerTimesOutAStuckDaemon(t *testing.T) {
	l := &containerLister{gap: time.Millisecond, timeout: 30 * time.Millisecond}
	f := &fakeFetch{took: time.Hour}

	start := time.Now()
	if _, err := l.get(context.Background(), false, f.fetch); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the listing's own deadline", err)
	}
	if time.Since(start) > time.Second {
		t.Error("a stuck daemon held the caller past the listing timeout")
	}
}

func TestEventFiltersDropHealthCheckExecs(t *testing.T) {
	f := eventFilters()
	for _, typ := range []string{"container", "image", "volume", "network"} {
		if !f.ExactMatch("type", typ) {
			t.Errorf("type %s is not relayed", typ)
		}
	}
	for _, a := range []string{"create", "start", "die", "destroy", "health_status", "pull"} {
		if !f.ExactMatch("event", a) {
			t.Errorf("event %s is not relayed", a)
		}
	}
	// The health-check noise. With health_status in the filter, Docker matches every action by
	// PREFIX (moby's events filter), so no relayed value may be a prefix of a noise action either.
	for _, a := range []string{"exec_create", "exec_start", "exec_die", "exec_detach", "attach", "resize", "top", "connect", "disconnect", "mount", "unmount", "kill", "stop"} {
		for _, v := range f.Get("event") {
			if strings.HasPrefix(a, v) {
				t.Errorf("event %s would be relayed (matches filter value %q)", a, v)
			}
		}
	}
}
