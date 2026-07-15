package limiter

import (
	"math"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// tokenBucket implements the token_bucket algorithm: Burst tokens of
// capacity, refilled continuously at Rate tokens per Window. Unlike the
// window algorithms it absorbs a burst up to capacity and then throttles
// to the steady refill rate, rather than admitting a fixed count per
// fixed interval.
type tokenBucket struct {
	tokens float64
	last   time.Time // zero until the first step; see the IsZero check below
}

func (b *tokenBucket) step(limit config.Limit, now time.Time) Decision {
	burst := float64(limit.Burst)
	refillPerSec := float64(limit.Rate) / time.Duration(limit.Window).Seconds()

	switch {
	case b.last.IsZero():
		// First request for this key: start full, same as a fresh
		// token_bucket key would in any implementation.
		b.tokens = burst
		b.last = now
	case now.After(b.last):
		b.tokens = math.Min(burst, b.tokens+now.Sub(b.last).Seconds()*refillPerSec)
		b.last = now
	default:
		// now <= last: no elapsed time (repeat timestamp) or the clock
		// went backward. Either way, grant no free refill, and leave
		// b.last alone so a later, correctly-ordered call still measures
		// elapsed time from the last known-good instant.
	}

	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	remaining := int(math.Floor(b.tokens))
	if remaining < 0 {
		remaining = 0
	}

	dec := Decision{
		Allowed:   allowed,
		Limit:     limit.Burst,
		Remaining: remaining,
		Reset:     now.Add(secondsToDuration((burst - b.tokens) / refillPerSec)),
	}
	if !allowed {
		dec.RetryAfter = secondsToDuration((1 - b.tokens) / refillPerSec)
	}
	return dec
}

// secondsToDuration converts a (possibly negative, from floating-point
// noise once the bucket is already full) seconds value to a Duration,
// clamped at zero.
func secondsToDuration(seconds float64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
