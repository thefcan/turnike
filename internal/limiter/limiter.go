package limiter

import (
	"context"
	"fmt"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// Decision is the outcome of a rate-limit check.
type Decision struct {
	Allowed bool

	// Limit is the request's quota: rate for the window algorithms,
	// burst (bucket capacity) for token_bucket — X-RateLimit-Limit.
	Limit int
	// Remaining is the quota left in the current window/bucket —
	// X-RateLimit-Remaining.
	Remaining int
	// Reset is when the quota next becomes fully available —
	// X-RateLimit-Reset.
	Reset time.Time
	// RetryAfter is how long to wait before retrying; meaningful only
	// when Allowed is false — Retry-After.
	RetryAfter time.Duration
}

// Limiter decides whether a request identified by key is allowed under
// limit. Implementations must be safe for concurrent use. The Redis
// backend (M3) implements this same signature via EVALSHA.
type Limiter interface {
	Allow(ctx context.Context, key string, limit config.Limit) (Decision, error)
}

// Clock abstracts time so algorithm state transitions are deterministic
// in tests. The Redis backend (M3) sources time from Redis TIME instead.
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock, backed by time.Now.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// New builds the Limiter selected by cfg.Backend. cfg is assumed to be
// config-validated (Backend is "memory" or "redis").
func New(cfg config.Limiter, clock Clock) (Limiter, error) {
	switch cfg.Backend {
	case config.BackendMemory:
		return NewMemoryLimiter(clock), nil
	case config.BackendRedis:
		return nil, fmt.Errorf("limiter: redis backend lands in M3")
	default:
		// Unreachable after config validation, but don't silently no-op.
		return nil, fmt.Errorf("limiter: unknown backend %q", cfg.Backend)
	}
}
