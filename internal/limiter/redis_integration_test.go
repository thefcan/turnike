package limiter

// Integration tests against a real redis (the Lua scripts' actual
// behavior: parity with the in-memory algorithms, TTLs, NOSCRIPT
// self-healing, and zero over-admission under concurrent instances).
//
// Gated on REDIS_ADDR: unset skips (plain `make test` stays
// redis-free), set-but-unreachable FAILS so CI can never silently skip.
// Windows are restricted to values that divide the Go-zero-time to
// Unix-epoch offset (1s / 1m / 1h) so the epoch-anchored redis grid and
// the Truncate-anchored memory grid coincide, and long enough that a
// grid boundary cannot land mid-test.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// newIntegrationLimiter returns a RedisLimiter against $REDIS_ADDR,
// skipping the test when the variable is unset.
func newIntegrationLimiter(t *testing.T) *RedisLimiter {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; redis integration test skipped")
	}
	l := NewRedisLimiter(config.Redis{Addr: addr}, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = l.Close() })
	if err := l.Ping(context.Background()); err != nil {
		t.Fatalf("REDIS_ADDR=%q is set but redis is unreachable: %v", addr, err)
	}
	return l
}

// integrationKey returns a per-run-unique gateway key and cleans up the
// redis state it maps to. Never FLUSHDB here - the dev redis is shared.
func integrationKey(t *testing.T, l *RedisLimiter, algo string) string {
	t.Helper()
	key := fmt.Sprintf("it:%s:%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = l.client.Del(context.Background(), "turnike:"+algo+":"+key).Err()
	})
	return key
}

func TestRedisFixedWindowParity(t *testing.T) {
	l := newIntegrationLimiter(t)
	key := integrationKey(t, l, config.AlgoFixedWindow)
	window := time.Hour
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 3, Window: config.Duration(window)}
	ctx := context.Background()

	var reset time.Time
	for i, wantRemaining := range []int{2, 1, 0} {
		dec, err := l.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("allow %d: %v", i+1, err)
		}
		if !dec.Allowed {
			t.Fatalf("allow %d: denied, want allowed", i+1)
		}
		if dec.Limit != limit.Rate {
			t.Errorf("allow %d: Limit = %d, want %d", i+1, dec.Limit, limit.Rate)
		}
		if dec.Remaining != wantRemaining {
			t.Errorf("allow %d: Remaining = %d, want %d", i+1, dec.Remaining, wantRemaining)
		}
		if dec.RetryAfter != 0 {
			t.Errorf("allow %d: RetryAfter = %v, want 0 on the allow path", i+1, dec.RetryAfter)
		}
		if i == 0 {
			reset = dec.Reset
			// The grid anchors at the Unix epoch, so the window end
			// must land exactly on the hour grid.
			if reset.UnixMicro()%int64(window/time.Microsecond) != 0 {
				t.Errorf("Reset %v is not on the epoch-anchored %v grid", reset, window)
			}
		} else if !dec.Reset.Equal(reset) {
			t.Errorf("allow %d: Reset moved within the window: %v, first saw %v", i+1, dec.Reset, reset)
		}
	}

	// Denied requests: correct decision fields and - the parity point -
	// no counting. The memory backend does not increment on deny.
	for i := 0; i < 2; i++ {
		dec, err := l.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("deny %d: %v", i+1, err)
		}
		if dec.Allowed {
			t.Fatalf("deny %d: allowed past rate, want denied", i+1)
		}
		if dec.Remaining != 0 {
			t.Errorf("deny %d: Remaining = %d, want 0", i+1, dec.Remaining)
		}
		if !dec.Reset.Equal(reset) {
			t.Errorf("deny %d: Reset = %v, want the window's %v", i+1, dec.Reset, reset)
		}
		if dec.RetryAfter <= 0 || dec.RetryAfter > window {
			t.Errorf("deny %d: RetryAfter = %v, want in (0, %v]", i+1, dec.RetryAfter, window)
		}
	}

	stateKey := "turnike:" + config.AlgoFixedWindow + ":" + key
	count, err := l.client.HGet(ctx, stateKey, "count").Int()
	if err != nil {
		t.Fatalf("HGET count: %v", err)
	}
	if count != limit.Rate {
		t.Errorf("stored count after denies = %d, want %d (deny must not increment)", count, limit.Rate)
	}
	ttl, err := l.client.PTTL(ctx, stateKey).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl <= 0 || ttl > window {
		t.Errorf("PTTL = %v, want in (0, %v] (every key must age out)", ttl, window)
	}
}

func TestRedisFixedWindowRollover(t *testing.T) {
	l := newIntegrationLimiter(t)
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Second)}
	ctx := context.Background()

	// Two back-to-back calls can straddle a 1s grid boundary; that makes
	// the second call *correctly* allowed. Straddling is a setup hazard,
	// not the property under test - retry with a fresh key when it hits.
	for attempt := 0; attempt < 4; attempt++ {
		key := integrationKey(t, l, config.AlgoFixedWindow)
		first, err := l.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		if !first.Allowed {
			t.Fatalf("first request on a fresh key denied: %+v", first)
		}
		second, err := l.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if second.Allowed {
			continue // straddled the boundary; try again on a fresh key
		}
		if second.RetryAfter <= 0 || second.RetryAfter > time.Second {
			t.Errorf("deny RetryAfter = %v, want in (0, 1s]", second.RetryAfter)
		}
		// 1.1s of real time later the denying window is over on the
		// redis clock too (offsets don't matter, only elapsed time).
		time.Sleep(1100 * time.Millisecond)
		third, err := l.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("third: %v", err)
		}
		if !third.Allowed {
			t.Errorf("request after window rollover denied: %+v", third)
		}
		return
	}
	t.Fatal("straddled the 1s grid boundary on 4 consecutive attempts")
}

func TestRedisScriptFlushSelfHeals(t *testing.T) {
	l := newIntegrationLimiter(t)
	key := integrationKey(t, l, config.AlgoFixedWindow)
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 5, Window: config.Duration(time.Hour)}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if dec, err := l.Allow(ctx, key, limit); err != nil || !dec.Allowed {
			t.Fatalf("pre-flush allow %d: dec=%+v err=%v", i+1, dec, err)
		}
	}

	// Emulates the script-cache half of a redis restart: EVALSHA starts
	// returning NOSCRIPT until something re-loads the script.
	if err := l.client.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("SCRIPT FLUSH: %v", err)
	}

	for i := 0; i < 3; i++ {
		dec, err := l.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("post-flush allow %d: %v (limiting must self-heal via EVAL)", i+1, err)
		}
		if !dec.Allowed {
			t.Fatalf("post-flush allow %d: denied, want allowed", i+1)
		}
	}
	// Exactly rate admitted across the flush: the counter survived (the
	// flush clears only the script cache), so the 6th request is denied.
	dec, err := l.Allow(ctx, key, limit)
	if err != nil {
		t.Fatalf("post-flush deny: %v", err)
	}
	if dec.Allowed {
		t.Error("6th request allowed: state was lost across SCRIPT FLUSH")
	}
}

func TestRedisTokenBucketParity(t *testing.T) {
	l := newIntegrationLimiter(t)
	key := integrationKey(t, l, config.AlgoTokenBucket)
	// rate 1/1m: the refill accrued during a µs-scale test (~1e-4
	// tokens) cannot move floor(tokens), so the assertions stay exact.
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 1, Burst: 5, Window: config.Duration(time.Minute)}
	ctx := context.Background()

	for i, wantRemaining := range []int{4, 3, 2, 1, 0} {
		dec, err := l.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("allow %d: %v", i+1, err)
		}
		if !dec.Allowed {
			t.Fatalf("allow %d: denied, want the full burst admitted", i+1)
		}
		if dec.Limit != limit.Burst {
			t.Errorf("allow %d: Limit = %d, want Burst %d", i+1, dec.Limit, limit.Burst)
		}
		if dec.Remaining != wantRemaining {
			t.Errorf("allow %d: Remaining = %d, want %d", i+1, dec.Remaining, wantRemaining)
		}
	}

	dec, err := l.Allow(ctx, key, limit)
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if dec.Allowed {
		t.Fatal("6th request allowed past an empty bucket")
	}
	if dec.Remaining != 0 {
		t.Errorf("deny Remaining = %d, want 0", dec.Remaining)
	}
	// Next token lands one refill interval after the bucket emptied.
	if dec.RetryAfter <= 50*time.Second || dec.RetryAfter > time.Minute {
		t.Errorf("deny RetryAfter = %v, want in (50s, 1m]", dec.RetryAfter)
	}

	stateKey := "turnike:" + config.AlgoTokenBucket + ":" + key
	raw, err := l.client.HGet(ctx, stateKey, "tokens").Result()
	if err != nil {
		t.Fatalf("HGET tokens: %v", err)
	}
	tokens, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("stored tokens %q does not round-trip float64: %v", raw, err)
	}
	if tokens < 0 || tokens > 0.01 {
		t.Errorf("stored tokens = %v, want ~0 after draining the burst", tokens)
	}
	// TTL = time to refill to full: 5 tokens at 1/minute, minus dust.
	ttl, err := l.client.PTTL(ctx, stateKey).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl <= 4*time.Minute || ttl > 5*time.Minute {
		t.Errorf("PTTL = %v, want in (4m, 5m] (time to full)", ttl)
	}
}

func TestRedisTokenBucketNeverExceedsBurst(t *testing.T) {
	l := newIntegrationLimiter(t)
	key := integrationKey(t, l, config.AlgoTokenBucket)
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 1, Burst: 5, Window: config.Duration(time.Second)}
	ctx := context.Background()

	// Seed state 1000h in the past (the local clock is fine here - the
	// gap dwarfs any clock skew): the refill must clamp at burst, not
	// accumulate 3.6M tokens.
	stateKey := "turnike:" + config.AlgoTokenBucket + ":" + key
	past := time.Now().Add(-1000 * time.Hour).UnixMicro()
	if err := l.client.HSet(ctx, stateKey, "tokens", "1", "last_us", strconv.FormatInt(past, 10)).Err(); err != nil {
		t.Fatalf("seed HSET: %v", err)
	}

	dec, err := l.Allow(ctx, key, limit)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !dec.Allowed {
		t.Fatal("denied, want allowed from a refilled bucket")
	}
	if want := limit.Burst - 1; dec.Remaining != want {
		t.Errorf("Remaining = %d, want %d (refill clamped at burst)", dec.Remaining, want)
	}
}

func TestRedisTokenBucketRefill(t *testing.T) {
	l := newIntegrationLimiter(t)
	key := integrationKey(t, l, config.AlgoTokenBucket)
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 1, Burst: 1, Window: config.Duration(time.Second)}
	ctx := context.Background()

	first, err := l.Allow(ctx, key, limit)
	if err != nil || !first.Allowed {
		t.Fatalf("first: dec=%+v err=%v, want allowed (fresh bucket starts full)", first, err)
	}
	second, err := l.Allow(ctx, key, limit)
	if err != nil || second.Allowed {
		t.Fatalf("second: dec=%+v err=%v, want denied (bucket empty)", second, err)
	}
	if second.RetryAfter <= 0 || second.RetryAfter > time.Second {
		t.Errorf("deny RetryAfter = %v, want in (0, 1s]", second.RetryAfter)
	}
	// 1.1s of real time refills one whole token on the redis clock too.
	time.Sleep(1100 * time.Millisecond)
	third, err := l.Allow(ctx, key, limit)
	if err != nil || !third.Allowed {
		t.Fatalf("after refill: dec=%+v err=%v, want allowed", third, err)
	}
}

func TestRedisTokenBucketHammer(t *testing.T) {
	l := newIntegrationLimiter(t)
	const burst = 50
	const instances = 4
	const perInstance = 100
	// rate 1/1h: the refill accrued during the hammer (~1e-5 tokens)
	// keeps "exactly burst admitted" an honest assertion.
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 1, Burst: burst, Window: config.Duration(time.Hour)}
	key := integrationKey(t, l, config.AlgoTokenBucket)

	limiters := []*RedisLimiter{l}
	for i := 1; i < instances; i++ {
		li := NewRedisLimiter(config.Redis{Addr: os.Getenv("REDIS_ADDR")}, slog.New(slog.DiscardHandler))
		t.Cleanup(func() { _ = li.Close() })
		limiters = append(limiters, li)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for _, li := range limiters {
		for i := 0; i < perInstance; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dec, err := li.Allow(ctx, key, limit)
				if err != nil {
					t.Error(err)
					return
				}
				if dec.Allowed {
					allowed.Add(1)
				}
			}()
		}
	}
	wg.Wait()

	if got := allowed.Load(); got != burst {
		t.Errorf("allowed = %d over %d attempts from %d instances, want exactly %d",
			got, instances*perInstance, instances, burst)
	}
}

func TestRedisFixedWindowHammer(t *testing.T) {
	l := newIntegrationLimiter(t)
	const rate = 50
	const instances = 4
	const perInstance = 100
	// 1h window: with a short one, a grid boundary landing mid-hammer
	// makes up to 2×rate admissions *correct* fixed_window behavior - a
	// permanent flake. The hour grid puts that off the table; the
	// documented boundary edge has the rollover test to itself.
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: rate, Window: config.Duration(time.Hour)}
	key := integrationKey(t, l, config.AlgoFixedWindow)

	// Separate clients, not just goroutines: this is the multi-instance
	// claim - N gateways sharing one redis admit exactly rate, total.
	limiters := []*RedisLimiter{l}
	for i := 1; i < instances; i++ {
		li := NewRedisLimiter(config.Redis{Addr: os.Getenv("REDIS_ADDR")}, slog.New(slog.DiscardHandler))
		t.Cleanup(func() { _ = li.Close() })
		limiters = append(limiters, li)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for _, li := range limiters {
		for i := 0; i < perInstance; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dec, err := li.Allow(ctx, key, limit)
				if err != nil {
					t.Error(err)
					return
				}
				if dec.Allowed {
					allowed.Add(1)
				}
			}()
		}
	}
	wg.Wait()

	if got := allowed.Load(); got != rate {
		t.Errorf("allowed = %d over %d attempts from %d instances, want exactly %d",
			got, instances*perInstance, instances, rate)
	}
}
