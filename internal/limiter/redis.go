package limiter

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/thefcan/turnike/internal/config"
)

// Client I/O budgets. A rate-limit decision arriving later than a second
// is worthless to the request waiting on it, so the timeouts stay tight;
// the on_error policy decides what happens to the request instead.
const (
	redisDialTimeout  = time.Second
	redisReadTimeout  = time.Second
	redisWriteTimeout = time.Second
)

//go:embed scripts/fixed_window.lua
var fixedWindowLua string

//go:embed scripts/token_bucket.lua
var tokenBucketLua string

//go:embed scripts/sliding_window.lua
var slidingWindowLua string

// scriptEntry binds one algorithm's Lua script to the glue that feeds
// it and reads it back.
type scriptEntry struct {
	script *redis.Script
	// limitOf picks the Decision.Limit (X-RateLimit-Limit) value,
	// mirroring the in-memory algorithms: Rate for the window
	// algorithms, Burst for token_bucket.
	limitOf func(config.Limit) int
	// args builds ARGV. The window travels in whole microseconds, the
	// scripts' time unit.
	args func(limit config.Limit, windowMicros int64) []any
}

// scripts maps each algorithm to its atomic check-and-consume script.
var scripts = map[string]scriptEntry{
	config.AlgoFixedWindow: {
		script:  redis.NewScript(fixedWindowLua),
		limitOf: func(l config.Limit) int { return l.Rate },
		args: func(l config.Limit, windowMicros int64) []any {
			return []any{l.Rate, windowMicros}
		},
	},
	config.AlgoTokenBucket: {
		script:  redis.NewScript(tokenBucketLua),
		limitOf: func(l config.Limit) int { return l.Burst },
		args: func(l config.Limit, windowMicros int64) []any {
			return []any{l.Burst, l.Rate, windowMicros}
		},
	},
	config.AlgoSlidingWindow: {
		script:  redis.NewScript(slidingWindowLua),
		limitOf: func(l config.Limit) int { return l.Rate },
		args: func(l config.Limit, windowMicros int64) []any {
			// The nonce uniquifies zset members against same-µs accepts
			// (see the script header). Uniqueness, not secrecy, so
			// rand/v2 is the right tool.
			return []any{l.Rate, windowMicros, fmt.Sprintf("%016x", rand.Uint64())} // #nosec G404 -- member uniqueness, nothing security-sensitive
		},
	},
}

// RedisLimiter is the redis-backed Limiter: rate-limit state shared by
// every gateway instance, one script call per decision, time sourced
// from redis TIME inside the scripts so instances need not agree on a
// clock. Script dispatch uses EVALSHA with go-redis's EVAL fallback on
// NOSCRIPT - and EVAL re-populates the server's script cache, so a redis
// restart or SCRIPT FLUSH self-heals on the very next decision.
//
// Every decision goes through the circuit breaker regardless of policy;
// the on_error policy only decides what a failure (or an open circuit)
// means: degrade is handled here via fallback, fail_open/fail_closed by
// the gateway on the returned error.
type RedisLimiter struct {
	client   *redis.Client
	scripter redis.Scripter // == client in production; a fake in unit tests
	breaker  *breaker
	fallback Limiter                                      // in-memory stand-in while redis is down; nil unless on_error: degrade
	backend  interface{ SetActiveBackend(active string) } // who answered last; never nil (nop when unwired)
	logger   *slog.Logger
}

// NewRedisLimiter builds the redis backend for cfg. Construction never
// fails on an unreachable redis: crash-looping while redis is briefly
// down would be strictly worse than coming up under the failure policy.
// clock drives the breaker and the degrade fallback; decisions
// themselves only ever see redis TIME. ins hooks are optional (zero
// value = unobserved).
func NewRedisLimiter(cfg config.Redis, clock Clock, logger *slog.Logger, ins Instruments) *RedisLimiter {
	ins = ins.withDefaults()
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		DialTimeout:  redisDialTimeout,
		ReadTimeout:  redisReadTimeout,
		WriteTimeout: redisWriteTimeout,
		// The failure policy owns retry behavior; go-redis's hidden
		// retries would mask the failures it reacts to and multiply
		// worst-case decision latency.
		MaxRetries: -1,
	})
	l := &RedisLimiter{
		client:   client,
		scripter: client,
		breaker:  newBreaker(clock, logger, ins.Breaker),
		backend:  ins.Backend,
		logger:   logger,
	}
	// The configured backend is presumed active until a decision says
	// otherwise, so a scrape racing boot never reads "memory" for a
	// healthy redis deployment. Note the degrade fallback built below
	// must not flip this - it answers nothing until redis fails.
	l.backend.SetActiveBackend(config.BackendRedis)
	if cfg.OnError == config.OnErrorDegrade {
		// Built once at boot, not per breaker trip: a flapping breaker
		// must not churn allocations, and stale state self-heals anyway
		// (all three algorithms evict or refill by timestamp, so
		// anything older than a window is inert).
		l.fallback = NewMemoryLimiter(clock)
	}

	// Best-effort eager SCRIPT LOAD: a Lua syntax error surfaces at boot
	// instead of on the first request, and that request stays a 1-RTT
	// EVALSHA. Failures only warn - the runtime path self-heals via the
	// EVAL fallback regardless. The shared 1s budget means an
	// unreachable redis costs one dial timeout, not one per script.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for algo, entry := range scripts {
		if err := entry.script.Load(ctx, client).Err(); err != nil {
			logger.Warn("redis script load failed (will self-heal via EVAL)",
				"algorithm", algo, "err", err)
		}
	}
	return l
}

// Allow implements Limiter: one atomic check-and-consume script call.
func (l *RedisLimiter) Allow(ctx context.Context, key string, limit config.Limit) (Decision, error) {
	entry, ok := scripts[limit.Algorithm]
	if !ok {
		// Unreachable behind config validation once all three scripts
		// have landed; don't silently no-op meanwhile.
		return Decision{}, fmt.Errorf("limiter: no redis script for algorithm %q", limit.Algorithm)
	}
	windowMicros := int64(time.Duration(limit.Window) / time.Microsecond)
	if windowMicros <= 0 {
		return Decision{}, fmt.Errorf("limiter: window %v is below the redis scripts' 1µs resolution",
			time.Duration(limit.Window))
	}
	// The memory backend's state key under the settled turnike: prefix;
	// end to end that is turnike:{algo}:{route_prefix}:{identity}.
	stateKey := "turnike:" + limit.Algorithm + ":" + key
	var vals []int64
	err := l.breaker.do(func() error {
		var runErr error
		vals, runErr = entry.script.Run(ctx, l.scripter,
			[]string{stateKey}, entry.args(limit, windowMicros)...).Int64Slice()
		return runErr
	})
	if err != nil {
		if l.fallback != nil {
			// degrade: per-instance approximate limiting while redis is
			// unavailable. The headers stay real - they describe the
			// instance-local quota - and over-admission is bounded by
			// N_instances × limit. A residual fallback error (the
			// maxKeys cap) propagates to the gateway's fail-open branch:
			// redis, then memory, then open.
			dec, fbErr := l.fallback.Allow(ctx, key, limit)
			if fbErr != nil {
				return dec, fbErr
			}
			dec.Degraded = true
			l.backend.SetActiveBackend(config.BackendMemory)
			return dec, nil
		}
		return Decision{}, fmt.Errorf("limiter: redis %s: %w", limit.Algorithm, err)
	}
	// redis answered this call - under fail_open/fail_closed nothing
	// else ever answers, so error paths above leave the gauge alone.
	l.backend.SetActiveBackend(config.BackendRedis)
	return decisionFromReply(vals, entry.limitOf(limit))
}

// decisionFromReply maps a script's {allowed01, remaining, reset_us,
// retry_after_us} reply onto a Decision. reset_us is absolute epoch
// microseconds from the redis clock - the only clock in play.
func decisionFromReply(vals []int64, limit int) (Decision, error) {
	if len(vals) != 4 {
		return Decision{}, fmt.Errorf("limiter: redis script returned %d values, want 4", len(vals))
	}
	return Decision{
		Allowed:    vals[0] == 1,
		Limit:      limit,
		Remaining:  int(vals[1]),
		Reset:      time.UnixMicro(vals[2]),
		RetryAfter: time.Duration(vals[3]) * time.Microsecond,
	}, nil
}

// Ping reports whether redis is reachable; its method value satisfies
// proxy.ReadyCheck. It deliberately bypasses the breaker, like the
// boot-time script loads: a readiness probe must report ground truth,
// not a verdict up to one cooldown stale - and probe traffic must not
// feed the breaker's failure count while request traffic is quiet.
func (l *RedisLimiter) Ping(ctx context.Context) error {
	return l.client.Ping(ctx).Err()
}

// Close releases the redis client's connection pool.
func (l *RedisLimiter) Close() error {
	return l.client.Close()
}
