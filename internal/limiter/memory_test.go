package limiter

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// manualClock is a Clock double for deterministic tests; not itself
// concurrency-safe (tests either fix it before concurrent Allow calls or
// don't Advance concurrently with them).
type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time { return c.t }

func TestMemoryLimiterKeyIsolation(t *testing.T) {
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(10 * time.Second)}
	m := NewMemoryLimiter(&manualClock{t: time.Unix(1_000_000_000, 0)})
	ctx := context.Background()

	first, err := m.Allow(ctx, "alice", limit)
	if err != nil || !first.Allowed {
		t.Fatalf("alice's first request: dec=%+v err=%v, want allowed", first, err)
	}
	// Same key, second request: exhausted.
	second, err := m.Allow(ctx, "alice", limit)
	if err != nil || second.Allowed {
		t.Fatalf("alice's second request: dec=%+v err=%v, want denied", second, err)
	}
	// A different key must not share alice's bucket.
	bob, err := m.Allow(ctx, "bob", limit)
	if err != nil || !bob.Allowed {
		t.Fatalf("bob's first request: dec=%+v err=%v, want allowed", bob, err)
	}
}

func TestMemoryLimiterAlgorithmSwitchGetsFreshState(t *testing.T) {
	// A key_overrides entry that switches algorithm (config.Route.LimitFor
	// keeps such overrides self-contained) must not inherit the base
	// limit's bucket state.
	fixed := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Minute)}
	m := NewMemoryLimiter(&manualClock{t: time.Unix(1_000_000_000, 0)})
	ctx := context.Background()

	if _, err := m.Allow(ctx, "shared", fixed); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Allow(ctx, "shared", fixed); err != nil {
		t.Fatal(err)
	} // exhausts the fixed_window bucket for "shared"

	other := config.Limit{Algorithm: "other_algo", Rate: 1, Window: config.Duration(time.Minute)}
	if _, err := m.Allow(ctx, "shared", other); err == nil {
		t.Fatal("unimplemented algorithm: want error, got nil")
	}
}

func TestMemoryLimiterOverAdmission(t *testing.T) {
	// The advisor's concurrency-correctness check: under a fixed clock,
	// N goroutines hammering the same key must yield exactly Rate
	// allowed decisions — no more, regardless of scheduling. Run with
	// -race.
	const rate = 20
	const attempts = 200
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: rate, Window: config.Duration(time.Minute)}
	m := NewMemoryLimiter(&manualClock{t: time.Unix(1_000_000_000, 0)})
	ctx := context.Background()

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dec, err := m.Allow(ctx, "hammered", limit)
			if err != nil {
				t.Error(err)
				return
			}
			if dec.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != rate {
		t.Errorf("allowed = %d over %d concurrent attempts, want exactly %d", allowed, attempts, rate)
	}
}

func TestMemoryLimiterCapsDistinctKeys(t *testing.T) {
	// Identity isn't authenticated, so nothing stops a caller from
	// varying X-API-Key per request; the cap is what stands between that
	// and unbounded memory growth. Shrink it here instead of looping to
	// the real six-figure count.
	old := maxKeys
	maxKeys = 2
	t.Cleanup(func() { maxKeys = old })

	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Minute)}
	m := NewMemoryLimiter(&manualClock{t: time.Unix(1_000_000_000, 0)})
	ctx := context.Background()

	if _, err := m.Allow(ctx, "a", limit); err != nil {
		t.Fatalf("key a (within capacity): unexpected error: %v", err)
	}
	if _, err := m.Allow(ctx, "b", limit); err != nil {
		t.Fatalf("key b (fills capacity): unexpected error: %v", err)
	}
	if _, err := m.Allow(ctx, "c", limit); err == nil {
		t.Fatal("key c (beyond capacity): want error, got nil")
	}
	// The cap only gates brand-new keys; already-tracked ones are
	// unaffected once the map is full.
	if _, err := m.Allow(ctx, "a", limit); err != nil {
		t.Errorf("already-tracked key a after capacity reached: unexpected error: %v", err)
	}
}

func TestNew(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	t.Run("memory backend", func(t *testing.T) {
		lim, err := New(config.Limiter{Backend: config.BackendMemory}, RealClock{}, logger)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, ok := lim.(*MemoryLimiter); !ok {
			t.Errorf("New returned %T, want *MemoryLimiter", lim)
		}
	})

	t.Run("redis backend", func(t *testing.T) {
		// A dead loopback addr: construction must still succeed (the
		// eager script load is best-effort), it must just come up
		// under the failure policy instead of crash-looping.
		lim, err := New(config.Limiter{
			Backend: config.BackendRedis,
			Redis:   config.Redis{Addr: "127.0.0.1:1", OnError: config.OnErrorDegrade},
		}, RealClock{}, logger)
		if err != nil {
			t.Fatalf("New(redis) with unreachable addr: %v, want success", err)
		}
		rl, ok := lim.(*RedisLimiter)
		if !ok {
			t.Fatalf("New returned %T, want *RedisLimiter", lim)
		}
		if err := rl.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	t.Run("unknown backend", func(t *testing.T) {
		_, err := New(config.Limiter{Backend: "memcached"}, RealClock{}, logger)
		if err == nil {
			t.Fatal("New(memcached): want error, got nil")
		}
	})
}
