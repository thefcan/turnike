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

// maxKeys bounds MemoryLimiter.state. Identity isn't authenticated —
// X-API-Key is free-form client input (identity.go) — so without a cap,
// anyone can grow the map without limit just by varying the header per
// request, no elevated privilege required. A var, not a const, so tests
// can shrink it instead of looping to a six-figure count.
var maxKeys = 100_000

// MemoryLimiter is the in-memory Limiter backend: one bucket per
// (algorithm, key), guarded by a single mutex. A single lock is
// deliberate — turnike's route counts don't warrant sharding, and a lock
// per key would need its own eviction story for no measured benefit.
//
// The state map is capped at maxKeys entries rather than reaped on a
// timer: a bounded map needs no background goroutine or shutdown
// lifecycle, and a request for a brand-new key beyond the cap simply
// fails open (see Gateway) instead of being tracked. M3's Redis backend
// replaces this whole map with TTL'd keys, which removes the need for
// either a cap or a reaper.
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
		if len(m.state) >= maxKeys {
			return Decision{}, fmt.Errorf("limiter: at capacity (%d distinct keys)", maxKeys)
		}
		nb, err := newBucket(limit.Algorithm)
		if err != nil {
			return Decision{}, err
		}
		b = nb
		m.state[stateKey] = b
	}
	return b.step(limit, m.clock.Now()), nil
}

// newBucket constructs the state for algorithm.
func newBucket(algorithm string) (bucket, error) {
	switch algorithm {
	case config.AlgoFixedWindow:
		return &fixedWindowBucket{}, nil
	case config.AlgoTokenBucket:
		return &tokenBucket{}, nil
	case config.AlgoSlidingWindow:
		return &slidingWindowBucket{}, nil
	default:
		// Unreachable given config-validated Limit values.
		return nil, fmt.Errorf("limiter: unknown algorithm %q", algorithm)
	}
}
