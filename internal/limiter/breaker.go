package limiter

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Breaker tuning. Constants, not config knobs: operational tuning with
// no universally correct value, and every knob costs validation, docs
// and tests - the house precedent is maxKeys and the gateway's
// transport timeouts. A threshold of 5 tolerates a lone timeout blip;
// clock injection keeps both testable without being variables.
const breakerFailureThreshold = 5

// BreakerCooldown is how long an open circuit waits before letting a
// single probe through to redis. Exported because the gateway's
// fail_closed 503 derives its Retry-After from it - one constant, so
// the header can never drift from the actual re-probe interval.
const BreakerCooldown = time.Second

// errBreakerOpen short-circuits redis calls while the circuit is open.
// Policy code treats it like any other redis failure; it is never fed
// back into the breaker's own bookkeeping.
var errBreakerOpen = errors.New("limiter: redis circuit open")

const (
	stateClosed = iota
	stateOpen
	stateHalfOpen
)

// gauge is the breaker_state hook: prometheus.Gauge satisfies it, and
// a one-method local interface keeps the prometheus client out of
// this package while making a recording test fake trivial. The gauge
// exports the state constants above verbatim (0/1/2).
type gauge interface{ Set(float64) }

// nopGauge is the default when no instrumentation is wired.
type nopGauge struct{}

func (nopGauge) Set(float64) {}

// breaker is a minimal three-state circuit breaker: closed until
// breakerFailureThreshold consecutive failures, open (rejecting without
// touching redis) for BreakerCooldown, then half-open admitting exactly
// one probe whose outcome picks the next state. It exists so an outage
// costs roughly one timed-out call per cooldown instead of one per
// request.
type breaker struct {
	clock  Clock
	logger *slog.Logger
	gauge  gauge // never nil; nopGauge when unwired

	mu       sync.Mutex
	state    int
	failures int       // consecutive failures; meaningful in closed
	openedAt time.Time // meaningful in open
}

func newBreaker(clock Clock, logger *slog.Logger, g gauge) *breaker {
	if g == nil {
		g = nopGauge{}
	}
	b := &breaker{clock: clock, logger: logger, gauge: g}
	// Materialize closed so the series reads 0 before any traffic.
	b.gauge.Set(float64(stateClosed))
	return b
}

// setState is the single choke point for state writes, so the gauge
// can never miss a transition. Callers hold b.mu; Gauge.Set is one
// atomic store, so no lock-ordering concern arises.
func (b *breaker) setState(s int) {
	b.state = s
	b.gauge.Set(float64(s))
}

// do runs fn under the breaker's supervision and returns its error, or
// errBreakerOpen without running fn while redis is considered down.
func (b *breaker) do(fn func() error) error {
	if err := b.admit(); err != nil {
		return err
	}
	err := fn()
	b.record(err)
	return err
}

func (b *breaker) admit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateOpen:
		if b.clock.Now().Sub(b.openedAt) < BreakerCooldown {
			return errBreakerOpen
		}
		// Cooldown over: this caller becomes the single probe.
		b.setState(stateHalfOpen)
		return nil
	case stateHalfOpen:
		// A probe is already in flight; half-open *means* that.
		return errBreakerOpen
	default: // stateClosed
		return nil
	}
}

// neutral reports whether err proves nothing about redis health.
// context.Canceled means the caller hung up mid-call. DeadlineExceeded
// is deliberately not neutral: the client's own 1s budgets turn a hung
// redis into exactly that error, and it must count.
func neutral(err error) bool {
	return errors.Is(err, context.Canceled)
}

func (b *breaker) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		switch {
		case err == nil:
			b.failures = 0
		case neutral(err):
			// No evidence either way: neither counted nor resetting.
		default:
			b.failures++
			if b.failures >= breakerFailureThreshold {
				b.setState(stateOpen)
				b.openedAt = b.clock.Now()
				// Carry the underlying error: under the degrade policy a
				// permanent script bug would otherwise masquerade as an
				// outage while requests quietly succeed on the fallback.
				b.logger.Warn("redis circuit opened",
					"consecutive_failures", b.failures, "err", err)
			}
		}
	case stateHalfOpen:
		switch {
		case err == nil:
			b.setState(stateClosed)
			b.failures = 0
			b.logger.Info("redis circuit closed by successful probe")
		case neutral(err):
			// The probe was canceled and proved nothing. Fall back to
			// open with openedAt untouched, so the next caller re-probes
			// immediately instead of waiting out a fresh cooldown - and
			// so the breaker cannot wedge in half-open.
			b.setState(stateOpen)
		default:
			b.setState(stateOpen)
			b.openedAt = b.clock.Now()
			b.logger.Warn("redis circuit re-opened by failed probe", "err", err)
		}
	default: // stateOpen
		// A call admitted before a concurrent transition finished; its
		// verdict is stale. The next probe is at most a cooldown away.
	}
}
