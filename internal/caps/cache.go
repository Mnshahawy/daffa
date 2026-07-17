package caps

import (
	"context"
	"sync"
)

// Loader computes a user's effective scoped mask from durable storage. The store implements
// it; keeping it an interface here is what stops this package from importing store and store
// from importing this package back.
type Loader interface {
	EffectiveMask(ctx context.Context, userID string) (ScopedMask, error)
}

// Cache memoises effective masks. Daffa is a single process (agents dial in to it, they do
// not run their own copy), so an in-process map is exact rather than merely eventually
// right.
type Cache struct {
	load Loader

	mu sync.RWMutex
	m  map[string]ScopedMask
}

func NewCache(load Loader) *Cache {
	return &Cache{load: load, m: map[string]ScopedMask{}}
}

// Of returns the user's effective scoped mask, loading it on a miss.
//
// An error is returned, never a zero mask — a caller that treated "the database is down"
// as "this user has no capabilities" would be right about the denial and wrong about the
// reason, and would log a permissions failure for what is actually an outage.
func (c *Cache) Of(ctx context.Context, userID string) (ScopedMask, error) {
	c.mu.RLock()
	m, ok := c.m[userID]
	c.mu.RUnlock()
	if ok {
		return m, nil
	}

	m, err := c.load.EffectiveMask(ctx, userID)
	if err != nil {
		return ScopedMask{}, err
	}

	c.mu.Lock()
	c.m[userID] = m
	c.mu.Unlock()
	return m, nil
}

// Invalidate drops the whole cache. Every write to roles or role_members calls it — and so
// does deleting an environment, since a grant scoped to it has just become meaningless.
//
// Two decisions worth defending, because both look like laziness and neither is:
//
// It drops EVERYTHING rather than the affected users. Editing a role changes the mask of
// everyone holding it, so the precise version has to fan out through role_members at every
// call site — and the failure mode of forgetting one is a user silently keeping a
// permission an admin believes they revoked. Roles change approximately never, a refill is
// one small query per active user, and one unconditional rule cannot be forgotten by
// whoever adds the next mutation.
//
// It DELETES rather than rebuilding. Invalidation runs after the write commits; a rebuild
// racing that commit can read pre-commit state and re-cache the stale mask with a fresh
// lifetime, which is strictly worse than no cache at all. Deleting is always correct.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	clear(c.m)
	c.mu.Unlock()
}
