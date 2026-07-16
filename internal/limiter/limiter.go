package limiter

import (
	"context"
	"fmt"
	"log/slog"
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
	// Reset is when this key is back to a clean slate — nothing counted
	// against it — X-RateLimit-Reset. For fixed_window that's the end of
	// the current window; for token_bucket, when the bucket refills to
	// Burst; for sliding_window, when every currently-counted request
	// has aged out (the newest one's expiry, not the next one's — the
	// next slot to free is RetryAfter's question, not this one's).
	Reset time.Time
	// RetryAfter is how long until the *next* request would succeed;
	// meaningful only when Allowed is false — Retry-After.
	RetryAfter time.Duration

	// Degraded reports that this decision came from the degrade
	// fallback rather than the configured backend. Observability only
	// (it feeds the requests_total decision label); gateway behavior
	// never branches on it, and the X-RateLimit-* headers stay the
	// fallback's real, instance-local values.
	Degraded bool
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
// config-validated (Backend is "memory" or "redis"). clock drives the
// in-memory algorithms - the redis backend sources time from redis TIME
// instead - and logger carries backend diagnostics.
func New(cfg config.Limiter, clock Clock, logger *slog.Logger) (Limiter, error) {
	switch cfg.Backend {
	case config.BackendMemory:
		return NewMemoryLimiter(clock), nil
	case config.BackendRedis:
		return NewRedisLimiter(cfg.Redis, clock, logger), nil
	default:
		// Unreachable after config validation, but don't silently no-op.
		return nil, fmt.Errorf("limiter: unknown backend %q", cfg.Backend)
	}
}
