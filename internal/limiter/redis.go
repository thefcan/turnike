package limiter

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
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
// token_bucket and sliding_window land in the next commits.
var scripts = map[string]scriptEntry{
	config.AlgoFixedWindow: {
		script:  redis.NewScript(fixedWindowLua),
		limitOf: func(l config.Limit) int { return l.Rate },
		args: func(l config.Limit, windowMicros int64) []any {
			return []any{l.Rate, windowMicros}
		},
	},
}

// RedisLimiter is the redis-backed Limiter: rate-limit state shared by
// every gateway instance, one script call per decision, time sourced
// from redis TIME inside the scripts so instances need not agree on a
// clock. Script dispatch uses EVALSHA with go-redis's EVAL fallback on
// NOSCRIPT - and EVAL re-populates the server's script cache, so a redis
// restart or SCRIPT FLUSH self-heals on the very next decision.
type RedisLimiter struct {
	client   *redis.Client
	scripter redis.Scripter // == client in production; a fake in unit tests
	logger   *slog.Logger
}

// NewRedisLimiter builds the redis backend for cfg. Construction never
// fails on an unreachable redis: crash-looping while redis is briefly
// down would be strictly worse than coming up under the failure policy.
func NewRedisLimiter(cfg config.Redis, logger *slog.Logger) *RedisLimiter {
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
	l := &RedisLimiter{client: client, scripter: client, logger: logger}

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
	vals, err := entry.script.Run(ctx, l.scripter,
		[]string{stateKey}, entry.args(limit, windowMicros)...).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: redis %s: %w", limit.Algorithm, err)
	}
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
// proxy.ReadyCheck. A readiness probe must report ground truth, so this
// always goes straight to redis.
func (l *RedisLimiter) Ping(ctx context.Context) error {
	return l.client.Ping(ctx).Err()
}

// Close releases the redis client's connection pool.
func (l *RedisLimiter) Close() error {
	return l.client.Close()
}
