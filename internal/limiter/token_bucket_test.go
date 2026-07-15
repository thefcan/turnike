package limiter

import (
	"testing"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// TestTokenBucketBurstAndRefill drives a single bucket through a
// deliberately exact-in-binary-floating-point timeline (halves and
// quarters of a second) so every Remaining/RetryAfter assertion below
// can use plain equality instead of an epsilon comparison.
func TestTokenBucketBurstAndRefill(t *testing.T) {
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 2, Burst: 5, Window: config.Duration(time.Second)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &tokenBucket{}

	type step struct {
		name          string
		at            time.Time
		wantAllowed   bool
		wantRemaining int
		wantRetryAft  time.Duration
	}
	steps := []step{
		{"burst 1/5", t0, true, 4, 0},
		{"burst 2/5", t0, true, 3, 0},
		{"burst 3/5", t0, true, 2, 0},
		{"burst 4/5", t0, true, 1, 0},
		{"burst 5/5", t0, true, 0, 0},
		{"burst exhausted", t0, false, 0, 500 * time.Millisecond},
		{"partial refill still short of 1 token", t0.Add(250 * time.Millisecond), false, 0, 250 * time.Millisecond},
		{"refill completes a token", t0.Add(500 * time.Millisecond), true, 0, 0},
	}
	for _, s := range steps {
		dec := b.step(limit, s.at)
		if dec.Allowed != s.wantAllowed {
			t.Errorf("%s: Allowed = %v, want %v", s.name, dec.Allowed, s.wantAllowed)
		}
		if dec.Remaining != s.wantRemaining {
			t.Errorf("%s: Remaining = %d, want %d", s.name, dec.Remaining, s.wantRemaining)
		}
		if dec.RetryAfter != s.wantRetryAft {
			t.Errorf("%s: RetryAfter = %v, want %v", s.name, dec.RetryAfter, s.wantRetryAft)
		}
		if dec.Limit != limit.Burst {
			t.Errorf("%s: Limit = %d, want Burst %d", s.name, dec.Limit, limit.Burst)
		}
	}
}

func TestTokenBucketSubSecondWindow(t *testing.T) {
	// A 100ms refill window (rate 10 per window -> 100 tokens/sec) must
	// refill on millisecond granularity, not get truncated to whole
	// seconds.
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 10, Burst: 3, Window: config.Duration(100 * time.Millisecond)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &tokenBucket{}

	for i := 0; i < 3; i++ {
		if dec := b.step(limit, t0); !dec.Allowed {
			t.Fatalf("burst request %d: Allowed = false, want true", i)
		}
	}
	if dec := b.step(limit, t0); dec.Allowed {
		t.Fatal("4th immediate request: Allowed = true, want false (burst exhausted)")
	}
	// 10ms at 100 tokens/sec refills exactly 1 token.
	dec := b.step(limit, t0.Add(10*time.Millisecond))
	if !dec.Allowed {
		t.Fatal("request after a 10ms refill: Allowed = false, want true")
	}
}

func TestTokenBucketNeverExceedsBurst(t *testing.T) {
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 1, Burst: 4, Window: config.Duration(time.Second)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &tokenBucket{}

	b.step(limit, t0) // starts full (4), consumes 1 -> 3 tokens

	// An enormous gap must cap the refill at Burst, not overflow it.
	dec := b.step(limit, t0.Add(1000*time.Hour))
	if !dec.Allowed {
		t.Fatal("Allowed = false after a huge refill gap, want true")
	}
	if dec.Remaining != limit.Burst-1 {
		t.Errorf("Remaining = %d, want %d (capped at Burst, then one consumed)", dec.Remaining, limit.Burst-1)
	}
}

func TestTokenBucketClockGoingBackwardGrantsNoFreeRefill(t *testing.T) {
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 1, Burst: 1, Window: config.Duration(time.Second)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &tokenBucket{}

	b.step(limit, t0) // starts full (1), consumes it -> 0 tokens
	if dec := b.step(limit, t0.Add(-time.Hour)); dec.Allowed {
		t.Error("a request timestamped before the last one must not be granted a free refill")
	}
}

func TestTokenBucketToleratesFloatingPointNoise(t *testing.T) {
	// rate=1/window=3s doesn't divide evenly in binary floating point
	// (refillPerSec = 1/3 = 0.3333...); accumulating ten 300ms refills
	// should reach exactly 1.0 token in exact arithmetic, but float64
	// summation error alone can land it one ULP under. A strict >=1
	// comparison would wrongly deny the request that lands on that
	// boundary; tokenEpsilon must absorb it.
	limit := config.Limit{Algorithm: config.AlgoTokenBucket, Rate: 1, Burst: 1, Window: config.Duration(3 * time.Second)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &tokenBucket{}
	b.step(limit, t0) // starts full, consumes the only token -> 0

	now := t0
	for i := 0; i < 9; i++ {
		now = now.Add(300 * time.Millisecond)
		b.step(limit, now) // each still short of a full token; state keeps advancing
	}
	now = now.Add(300 * time.Millisecond) // 10th step: 3s elapsed in total, one full window
	if dec := b.step(limit, now); !dec.Allowed {
		t.Errorf("request after accumulating a full token across 10 sub-steps: Allowed = false, want true (tokens=%v short of 1.0)", 1-b.tokens)
	}
}
