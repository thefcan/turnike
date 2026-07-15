package limiter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// bucket is one algorithm's mutable state for a single key. step mutates
// the bucket in place and returns the decision for now; it is not
// concurrent-safe on its own — MemoryLimiter serializes access.
type bucket interface {
	step(limit config.Limit, now time.Time) Decision
}

// MemoryLimiter is the in-memory Limiter backend: one bucket per
// (algorithm, key), guarded by a single mutex. A single lock is
// deliberate — turnike's route counts don't warrant sharding, and a lock
// per key would need its own eviction story for no measured benefit.
//
// The state map grows by one entry per distinct key seen and is never
// evicted; a long-lived process accumulating unbounded distinct API keys
// or client IPs will grow memory without bound. Documented as a known M2
// limitation — M3's Redis backend replaces this map with TTL'd keys,
// which removes the problem rather than papering over it here.
type MemoryLimiter struct {
	clock Clock
	mu    sync.Mutex
	state map[string]bucket
}

// NewMemoryLimiter builds a MemoryLimiter that sources time from clock.
func NewMemoryLimiter(clock Clock) *MemoryLimiter {
	return &MemoryLimiter{clock: clock, state: make(map[string]bucket)}
}

// Allow implements Limiter.
func (m *MemoryLimiter) Allow(_ context.Context, key string, limit config.Limit) (Decision, error) {
	// The algorithm is part of the bucket's identity: a key_overrides
	// entry that switches algorithm must not reuse the base limit's
	// state (config.Route.LimitFor already keeps such overrides
	// self-contained; mirroring that split here avoids a token_bucket
	// bucket being misread as a window log or vice versa).
	stateKey := limit.Algorithm + ":" + key

	m.mu.Lock()
	defer m.mu.Unlock()

	b, ok := m.state[stateKey]
	if !ok {
		nb, err := newBucket(limit.Algorithm)
		if err != nil {
			return Decision{}, err
		}
		b = nb
		m.state[stateKey] = b
	}
	return b.step(limit, m.clock.Now()), nil
}

// newBucket constructs the state for algorithm. Cases are added one per
// milestone commit as each algorithm lands (sliding_window next); until
// then, config-valid algorithms not yet implemented fall through to the
// error below.
func newBucket(algorithm string) (bucket, error) {
	switch algorithm {
	case config.AlgoFixedWindow:
		return &fixedWindowBucket{}, nil
	case config.AlgoTokenBucket:
		return &tokenBucket{}, nil
	default:
		return nil, fmt.Errorf("limiter: algorithm %q not implemented yet", algorithm)
	}
}
