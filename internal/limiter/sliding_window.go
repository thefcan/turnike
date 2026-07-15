package limiter

import (
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// slidingWindowBucket implements the sliding_window algorithm: a log of
// accepted timestamps, evicting anything older than Window on every
// step. Unlike fixed_window it never admits more than Rate requests in
// *any* Window-length interval — including one straddling a fixed grid
// boundary — at the cost of remembering up to Rate timestamps per key
// instead of a single counter.
type slidingWindowBucket struct {
	timestamps []time.Time // ascending, bounded at Rate entries (never grows past it)
}

func (b *slidingWindowBucket) step(limit config.Limit, now time.Time) Decision {
	window := time.Duration(limit.Window)
	cutoff := now.Add(-window)

	// Evict in place: a timestamp exactly Window old no longer counts
	// (strict After), so it frees its slot on the tick it expires.
	// Assumes now is non-decreasing across calls for the same key, same
	// as the other algorithms — kept stays sorted ascending because
	// entries are only ever appended at the tail.
	n := 0
	for _, ts := range b.timestamps {
		if ts.After(cutoff) {
			b.timestamps[n] = ts
			n++
		}
	}
	b.timestamps = b.timestamps[:n]

	allowed := len(b.timestamps) < limit.Rate
	if allowed {
		b.timestamps = append(b.timestamps, now)
	}
	remaining := limit.Rate - len(b.timestamps)
	if remaining < 0 {
		remaining = 0
	}

	dec := Decision{
		Allowed:   allowed,
		Limit:     limit.Rate,
		Remaining: remaining,
		// Reset is when the log is completely clear again: the newest
		// counted entry's expiry, since it's the last to age out. This
		// differs from RetryAfter below on purpose — at capacity with
		// staggered requests, one slot frees well before all of them do.
		Reset: now,
	}
	if len(b.timestamps) > 0 {
		dec.Reset = b.timestamps[len(b.timestamps)-1].Add(window)
	}
	if !allowed {
		// The oldest counted entry is the next (and only the next) one
		// to age out, freeing exactly one slot.
		dec.RetryAfter = b.timestamps[0].Add(window).Sub(now)
	}
	return dec
}
