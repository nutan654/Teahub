package httpmw

import (
	"sync"
	"time"
)

// UserRateLimiter is a simple in-memory token-bucket rate limiter keyed by
// user ID. It's intentionally not distributed: on Cloud Run with multiple
// instances, the effective limit is (per-instance limit * instance count),
// which is a looser bound than the configured rate but still turns an
// unbounded cost-bearing endpoint into a bounded one. A real multi-instance
// guarantee would need a shared store (Redis/Postgres); see
// ARCHITECTURE.md "Known limitations" for the same tradeoff already made
// for OAuth state tokens.
type UserRateLimiter struct {
	ratePerSec float64
	burst      float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewUserRateLimiter builds a limiter allowing ratePerSec sustained
// requests per user, with burst as the maximum instantaneous allowance.
func NewUserRateLimiter(ratePerSec float64, burst int) *UserRateLimiter {
	return &UserRateLimiter{
		ratePerSec: ratePerSec,
		burst:      float64(burst),
		buckets:    make(map[string]*bucket),
	}
}

// Allow reports whether the caller identified by userID may proceed now,
// consuming one token if so.
func (l *UserRateLimiter) Allow(userID string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[userID]
	if !ok {
		b = &bucket{tokens: l.burst - 1, last: now}
		l.buckets[userID] = b
		return true
	}

	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.ratePerSec
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// StartCleanup periodically evicts buckets untouched for longer than
// staleAfter, so a limiter tracking many distinct users over a long
// process lifetime doesn't grow unbounded. Call once at startup; it runs
// until the process exits (there's no shutdown hook because the map is
// small and cheap enough that leaking the goroutine until process exit is
// an acceptable tradeoff for a simple background sweep).
func (l *UserRateLimiter) StartCleanup(interval, staleAfter time.Duration) {
	go func() {
		for range time.Tick(interval) {
			cutoff := time.Now().Add(-staleAfter)
			l.mu.Lock()
			for id, b := range l.buckets {
				if b.last.Before(cutoff) {
					delete(l.buckets, id)
				}
			}
			l.mu.Unlock()
		}
	}()
}
