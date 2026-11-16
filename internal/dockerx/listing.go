package dockerx

import (
	"context"
	"slices"
	"sync"
	"time"
)

// A container listing is NOT a cheap read, and the list view used to treat it as one.
//
// On a daemon using the containerd image store, `GET /containers/json` resolves every listed
// container's image with its own gRPC round trip to containerd. A full listing (`all=true`, with a
// swarm's stopped task containers in it) of a couple of hundred containers takes around a second.
// The UI asks for one whenever Docker reports an event, and every open tab, the Overview and the
// command palette ask independently. During a deploy that came to over a dozen full listings a
// second, against one daemon. An abandoned listing is no cheaper: the daemon finishes resolving
// every image anyway, logging a "context canceled" warning per container. Enough of these at
// once can wedge dockerd itself, and a wedged dockerd fails every health check on the machine.
//
// So the daemon answers ONE listing per node and per `all` at a time, however many people ask:
//
//   - While a listing runs, new requests do not join it. It may have started before the change
//     they are asking about, so they wait for the next one, and all share that. A plain TTL cache
//     was the other option, and it is wrong for the same reason: it serves the list from before
//     the container you just stopped, and nothing refreshes it after.
//   - Consecutive listings start at least listingGap apart, so a steady stream of requests costs
//     at most one listing per gap rather than a back-to-back loop.
//   - The listing runs on its OWN context, bounded by listingTimeout, not on the first caller's.
//     A closed tab must not cancel a listing others are waiting on, and cancelling saves the
//     daemon nothing anyway.
//   - A listing nobody is waiting for by the time it would start is skipped.
//   - The last good result is kept, so a daemon that stops answering can still be shown as it
//     was (marked as a snapshot) instead of as a 502. See LastContainers.
const (
	listingTimeout = 10 * time.Second
	listingGap     = time.Second
	// How old a snapshot may be and still stand in for a daemon that did not answer. Beyond this,
	// "this is how it looked" stops being useful and the failure is the better answer.
	snapshotMaxAge = 5 * time.Minute
)

type listCall struct {
	done    chan struct{}
	list    []Container
	err     error
	waiters int
	started bool // set when the daemon is actually asked; until then, newcomers can still join
}

type listSlot struct {
	running   *listCall
	queued    *listCall
	lastStart time.Time
	last      []Container
	lastAt    time.Time
}

// containerLister coordinates the listings of one node. Its zero value is ready to use.
type containerLister struct {
	mu    sync.Mutex
	slots [2]listSlot // [0] running only, [1] all

	// Zero means the package defaults; tests shorten them.
	gap, timeout time.Duration
}

func slotIndex(all bool) int {
	if all {
		return 1
	}
	return 0
}

// get returns a listing that started after the call did, fetched by fetch. ctx bounds only how
// long THIS caller waits; the listing itself carries on for the others.
func (l *containerLister) get(ctx context.Context, all bool, fetch func(context.Context, bool) ([]Container, error)) ([]Container, error) {
	l.mu.Lock()
	s := &l.slots[slotIndex(all)]
	var c *listCall
	switch {
	case s.running == nil:
		c = &listCall{done: make(chan struct{})}
		s.running = c
		go l.run(s, c, all, fetch)
	case !s.running.started:
		// Still waiting out the gap, so it will start after this caller arrived: fresh enough.
		c = s.running
	case s.queued != nil:
		c = s.queued
	default:
		c = &listCall{done: make(chan struct{})}
		s.queued = c
	}
	c.waiters++
	l.mu.Unlock()

	select {
	case <-c.done:
		if c.err != nil {
			return nil, c.err
		}
		// A copy: callers tag rows in place (fan-out sets Node), and the rows are shared.
		return slices.Clone(c.list), nil
	case <-ctx.Done():
		l.mu.Lock()
		c.waiters--
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

// run executes c, then whatever was queued behind it, until the slot is idle.
func (l *containerLister) run(s *listSlot, c *listCall, all bool, fetch func(context.Context, bool) ([]Container, error)) {
	gap, timeout := l.gap, l.timeout
	if gap == 0 {
		gap = listingGap
	}
	if timeout == 0 {
		timeout = listingTimeout
	}

	for c != nil {
		l.mu.Lock()
		wait := gap - time.Since(s.lastStart)
		l.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}

		l.mu.Lock()
		if c.waiters == 0 {
			// Everyone gave up before it started: do not spend the daemon on a list nobody reads.
			c.err = context.Canceled
		} else {
			c.started = true
			s.lastStart = time.Now()
			l.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			list, err := fetch(ctx, all)
			cancel()

			l.mu.Lock()
			c.list, c.err = list, err
			if err == nil {
				s.last, s.lastAt = list, time.Now()
			}
		}
		close(c.done)
		c, s.queued = s.queued, nil
		s.running = c
		l.mu.Unlock()
	}
}

// snapshot returns the last good listing, if there is one recent enough to stand in.
func (l *containerLister) snapshot(all bool) ([]Container, time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := &l.slots[slotIndex(all)]
	if s.last == nil || time.Since(s.lastAt) > snapshotMaxAge {
		return nil, time.Time{}, false
	}
	return slices.Clone(s.last), s.lastAt, true
}
