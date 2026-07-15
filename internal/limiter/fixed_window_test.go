package limiter

import (
	"testing"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

func TestFixedWindowBucket(t *testing.T) {
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 3, Window: config.Duration(10 * time.Second)}
	epoch := time.Unix(1_000_000_000, 0).UTC() // arbitrary fixed instant, not window-aligned by construction
	b := &fixedWindowBucket{}

	// First three requests in the window are allowed, with Remaining
	// counting down and Reset held constant.
	var firstReset time.Time
	for i := 0; i < 3; i++ {
		dec := b.step(limit, epoch.Add(time.Duration(i)*time.Second))
		if !dec.Allowed {
			t.Fatalf("request %d: Allowed = false, want true", i)
		}
		if want := 3 - (i + 1); dec.Remaining != want {
			t.Errorf("request %d: Remaining = %d, want %d", i, dec.Remaining, want)
		}
		if dec.Limit != 3 {
			t.Errorf("request %d: Limit = %d, want 3", i, dec.Limit)
		}
		if i == 0 {
			firstReset = dec.Reset
		} else if !dec.Reset.Equal(firstReset) {
			t.Errorf("request %d: Reset = %v, want stable %v within the window", i, dec.Reset, firstReset)
		}
	}

	// The 4th request in the same window is denied with a positive
	// RetryAfter that lands exactly on Reset.
	deny := b.step(limit, epoch.Add(2*time.Second))
	if deny.Allowed {
		t.Fatal("4th request in window: Allowed = true, want false")
	}
	if deny.Remaining != 0 {
		t.Errorf("denied Remaining = %d, want 0", deny.Remaining)
	}
	if deny.RetryAfter <= 0 {
		t.Errorf("denied RetryAfter = %v, want > 0", deny.RetryAfter)
	}
	if got := epoch.Add(2 * time.Second).Add(deny.RetryAfter); !got.Equal(deny.Reset) {
		t.Errorf("now+RetryAfter = %v, want Reset %v", got, deny.Reset)
	}

	// Once the window rolls over, the quota is fully available again.
	next := b.step(limit, firstReset.Add(time.Millisecond))
	if !next.Allowed {
		t.Fatal("first request in new window: Allowed = false, want true")
	}
	if next.Remaining != 2 {
		t.Errorf("first request in new window: Remaining = %d, want 2", next.Remaining)
	}
	if !next.Reset.After(firstReset) {
		t.Errorf("new window Reset = %v, want after previous Reset %v", next.Reset, firstReset)
	}
}

func TestFixedWindowBucketAlignsToGrid(t *testing.T) {
	// Two independent buckets seeing requests at different offsets
	// within the same 10s grid cell must agree on the window boundary
	// (Truncate aligns to a fixed grid, not to each bucket's first
	// request).
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(10 * time.Second)}
	base := time.Unix(1_000_000_000, 0).UTC().Truncate(10 * time.Second)

	a := &fixedWindowBucket{}
	decA := a.step(limit, base.Add(1*time.Second))
	c := &fixedWindowBucket{}
	decC := c.step(limit, base.Add(9*time.Second))

	if !decA.Reset.Equal(decC.Reset) {
		t.Errorf("Reset misaligned across the same grid cell: %v vs %v", decA.Reset, decC.Reset)
	}
}
