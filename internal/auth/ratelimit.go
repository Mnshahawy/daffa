package auth

import (
	"sync"
	"time"
)

// Limiter throttles password attempts per key (username+IP). It is in-memory and
// per-process: Daffa runs as a single instance, and a limiter that survives restarts
// would just be a lockout waiting to be someone's outage.
type Limiter struct {
	mu       sync.Mutex
	attempts map[string]*bucket
	max      int
	window   time.Duration
}

type bucket struct {
	count int
	until time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{attempts: map[string]*bucket{}, max: max, window: window}
}

// Allow reports whether another attempt may be made for key.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.attempts[key]
	if !ok || time.Now().After(b.until) {
		return true
	}
	return b.count < l.max
}

// Fail records a failed attempt.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.attempts[key]
	if !ok || time.Now().After(b.until) {
		l.attempts[key] = &bucket{count: 1, until: time.Now().Add(l.window)}
		return
	}
	b.count++
}

// Reset clears the counter after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}
