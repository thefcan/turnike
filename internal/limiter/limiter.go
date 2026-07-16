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

// Instruments carries the limiter's observability hooks as small
// structural interfaces - satisfied by the metrics package's
// instruments - so this package never imports the metrics package or
// the prometheus client. The zero value is valid: nil hooks become
// no-ops.
type Instruments struct {
	// Breaker receives the circuit breaker state as the breaker's own
	// constants (0 closed, 1 open, 2 half-open) on every transition.
	Breaker interface{ Set(float64) }
	// Backend receives the backend that answered the most recent
	// decision (config.BackendRedis / config.BackendMemory).
	Backend interface{ SetActiveBackend(active string) }
}

type nopBackend struct{}

func (nopBackend) SetActiveBackend(string) {}

// withDefaults replaces nil hooks with no-ops so constructors and
// tests can pass a zero Instruments.
func (ins Instruments) withDefaults() Instruments {
	if ins.Breaker == nil {
		ins.Breaker = nopGauge{}
	}
	if ins.Backend == nil {
		ins.Backend = nopBackend{}
	}
	return ins
}

// New builds the Limiter selected by cfg.Backend. cfg is assumed to be
// config-validated (Backend is "memory" or "redis"). clock drives the
// in-memory algorithms - the redis backend sources time from redis TIME
// instead - and logger carries backend diagnostics. ins hooks are
// optional (zero value = unobserved).
func New(cfg config.Limiter, clock Clock, logger *slog.Logger, ins Instruments) (Limiter, error) {
	ins = ins.withDefaults()
	switch cfg.Backend {
	case config.BackendMemory:
		// The memory backend is the primary here, not a fallback; mark
		// it active once. No breaker exists, so the breaker gauge
		// keeps reporting its registered value of 0 (closed).
		ins.Backend.SetActiveBackend(config.BackendMemory)
		return NewMemoryLimiter(clock), nil
	case config.BackendRedis:
		return NewRedisLimiter(cfg.Redis, clock, logger, ins), nil
	default:
		// Unreachable after config validation, but don't silently no-op.
		return nil, fmt.Errorf("limiter: unknown backend %q", cfg.Backend)
	}
}
