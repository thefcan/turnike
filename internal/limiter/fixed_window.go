package limiter

import (
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// fixedWindowBucket implements the fixed_window algorithm: a request
// counter that resets every Window. Simple and cheap, at the cost of
// letting up to 2×Rate requests through across a window boundary (half
// at the end of one window, half at the start of the next) — the
// tradeoff sliding_window exists to avoid.
type fixedWindowBucket struct {
	windowStart time.Time
	count       int
}

func (b *fixedWindowBucket) step(limit config.Limit, now time.Time) Decision {
	window := time.Duration(limit.Window)
	// Truncate aligns windowStart to a fixed grid (relative to the Go
	// zero time) rather than sliding from this key's first request, so
	// concurrent keys sharing a window boundary reset in lockstep.
	windowStart := now.Truncate(window)
	if !windowStart.Equal(b.windowStart) {
		b.windowStart = windowStart
		b.count = 0
	}

	reset := windowStart.Add(window)
	allowed := b.count < limit.Rate
	if allowed {
		b.count++
	}
	remaining := limit.Rate - b.count
	if remaining < 0 {
		remaining = 0
	}

	dec := Decision{
		Allowed:   allowed,
		Limit:     limit.Rate,
		Remaining: remaining,
		Reset:     reset,
	}
	if !allowed {
		dec.RetryAfter = reset.Sub(now)
	}
	return dec
}
