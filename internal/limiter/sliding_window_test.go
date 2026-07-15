package limiter

import (
	"testing"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

func TestSlidingWindowBucket(t *testing.T) {
	limit := config.Limit{Algorithm: config.AlgoSlidingWindow, Rate: 3, Window: config.Duration(10 * time.Second)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &slidingWindowBucket{}

	type step struct {
		name          string
		at            time.Time
		wantAllowed   bool
		wantRemaining int
	}
	steps := []step{
		{"1st of 3", t0, true, 2},
		{"2nd of 3", t0.Add(1 * time.Second), true, 1},
		{"3rd of 3", t0.Add(2 * time.Second), true, 0},
		{"4th within the same 10s window", t0.Add(3 * time.Second), false, 0},
	}
	for _, s := range steps {
		dec := b.step(limit, s.at)
		if dec.Allowed != s.wantAllowed {
			t.Errorf("%s: Allowed = %v, want %v", s.name, dec.Allowed, s.wantAllowed)
		}
		if dec.Remaining != s.wantRemaining {
			t.Errorf("%s: Remaining = %d, want %d", s.name, dec.Remaining, s.wantRemaining)
		}
	}

	// Denied at t0+3s: the oldest counted request (t0) exits the window
	// at t0+10s, so that's exactly Reset/RetryAfter.
	deny := b.step(limit, t0.Add(3*time.Second))
	wantReset := t0.Add(10 * time.Second)
	if !deny.Reset.Equal(wantReset) {
		t.Errorf("Reset = %v, want %v", deny.Reset, wantReset)
	}
	if want := 7 * time.Second; deny.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v", deny.RetryAfter, want)
	}

	// Just after the oldest entry (t0) ages out, its slot frees up.
	next := b.step(limit, t0.Add(10*time.Second).Add(time.Millisecond))
	if !next.Allowed {
		t.Fatal("request just past the oldest entry's expiry: Allowed = false, want true")
	}
}

func TestSlidingWindowExactBoundaryIsEvicted(t *testing.T) {
	// A timestamp exactly Window old must no longer count (strict
	// After), so it frees its slot on the tick it expires rather than
	// one tick later.
	limit := config.Limit{Algorithm: config.AlgoSlidingWindow, Rate: 1, Window: config.Duration(10 * time.Second)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &slidingWindowBucket{}

	if dec := b.step(limit, t0); !dec.Allowed {
		t.Fatal("1st request: Allowed = false, want true")
	}
	if dec := b.step(limit, t0.Add(5*time.Second)); dec.Allowed {
		t.Fatal("2nd request at +5s (still within the 10s window): Allowed = true, want false")
	}
	if dec := b.step(limit, t0.Add(10*time.Second)); !dec.Allowed {
		t.Fatal("3rd request at exactly +10s (the 1st entry's exact expiry): Allowed = false, want true")
	}
}

func TestSlidingWindowIsExactAcrossBoundary(t *testing.T) {
	// Unlike fixed_window, sliding_window must never admit more than
	// Rate requests in any Window-length interval, including one that
	// straddles where a fixed grid would have reset.
	limit := config.Limit{Algorithm: config.AlgoSlidingWindow, Rate: 2, Window: config.Duration(10 * time.Second)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &slidingWindowBucket{}

	if dec := b.step(limit, t0.Add(9*time.Second)); !dec.Allowed {
		t.Fatal("request at +9s: Allowed = false, want true")
	}
	if dec := b.step(limit, t0.Add(9500*time.Millisecond)); !dec.Allowed {
		t.Fatal("request at +9.5s: Allowed = false, want true")
	}
	// +10.2s: both prior requests (+9s, +9.5s) are still within the
	// preceding 10s ([+0.2s, +10.2s]). A fixed_window with a 10s grid
	// rooted at t0 would have reset at +10s and allowed this.
	if dec := b.step(limit, t0.Add(10200*time.Millisecond)); dec.Allowed {
		t.Fatal("request at +10.2s: Allowed = true, want false (would over-admit across the boundary)")
	}
}

func TestSlidingWindowBucketMemoryStaysBoundedAtRate(t *testing.T) {
	// The timestamp log for one key never grows past Rate entries: once
	// full, further requests are denied (not appended) until eviction.
	limit := config.Limit{Algorithm: config.AlgoSlidingWindow, Rate: 5, Window: config.Duration(time.Second)}
	t0 := time.Unix(1_000_000_000, 0)
	b := &slidingWindowBucket{}

	for i := 0; i < 50; i++ {
		b.step(limit, t0) // same instant: only the first 5 are ever admitted
	}
	if len(b.timestamps) != limit.Rate {
		t.Errorf("len(timestamps) = %d, want capped at Rate=%d", len(b.timestamps), limit.Rate)
	}
}
