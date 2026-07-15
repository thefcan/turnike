package limiter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

var errRedisDown = errors.New("dial tcp: connection refused")

func newTestBreaker() (*breaker, *manualClock) {
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	return newBreaker(clock, slog.New(slog.DiscardHandler)), clock
}

// failN drives the breaker to the open state with threshold failures.
func trip(t *testing.T, b *breaker) {
	t.Helper()
	for i := 0; i < breakerFailureThreshold; i++ {
		if err := b.do(func() error { return errRedisDown }); !errors.Is(err, errRedisDown) {
			t.Fatalf("failure %d: err = %v, want the fn error while closed", i+1, err)
		}
	}
}

func TestBreakerTripsAfterConsecutiveFailures(t *testing.T) {
	b, _ := newTestBreaker()
	trip(t, b)

	called := false
	err := b.do(func() error { called = true; return nil })
	if !errors.Is(err, errBreakerOpen) {
		t.Fatalf("err = %v, want errBreakerOpen after %d failures", err, breakerFailureThreshold)
	}
	if called {
		t.Fatal("fn was called while the circuit was open")
	}
}

func TestBreakerSuccessResetsFailureCount(t *testing.T) {
	b, _ := newTestBreaker()
	for i := 0; i < breakerFailureThreshold-1; i++ {
		_ = b.do(func() error { return errRedisDown })
	}
	if err := b.do(func() error { return nil }); err != nil {
		t.Fatalf("success while closed: %v", err)
	}
	// The reset means another threshold-1 failures still don't trip it.
	for i := 0; i < breakerFailureThreshold-1; i++ {
		if err := b.do(func() error { return errRedisDown }); !errors.Is(err, errRedisDown) {
			t.Fatalf("post-reset failure %d: err = %v, want the fn error (still closed)", i+1, err)
		}
	}
	if err := b.do(func() error { return errRedisDown }); !errors.Is(err, errRedisDown) {
		t.Fatalf("threshold-th failure: err = %v, want the fn error (this one trips it)", err)
	}
	if err := b.do(func() error { return nil }); !errors.Is(err, errBreakerOpen) {
		t.Fatalf("err = %v, want errBreakerOpen after the fresh streak", err)
	}
}

func TestBreakerCanceledCallIsNeutralInClosed(t *testing.T) {
	b, _ := newTestBreaker()
	for i := 0; i < breakerFailureThreshold-1; i++ {
		_ = b.do(func() error { return errRedisDown })
	}
	// Canceled: neither the fifth failure nor a streak reset.
	if err := b.do(func() error { return context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call: err = %v, want context.Canceled passed through", err)
	}
	called := false
	_ = b.do(func() error { called = true; return errRedisDown })
	if !called {
		t.Fatal("circuit opened on a canceled call: cancellation proves nothing about redis")
	}
	// That call was the real threshold-th failure.
	if err := b.do(func() error { return nil }); !errors.Is(err, errBreakerOpen) {
		t.Fatalf("err = %v, want errBreakerOpen", err)
	}
}

func TestBreakerCooldownProbeClosesOnSuccess(t *testing.T) {
	b, clock := newTestBreaker()
	trip(t, b)

	// Mid-cooldown: still rejecting.
	clock.t = clock.t.Add(BreakerCooldown / 2)
	if err := b.do(func() error { return nil }); !errors.Is(err, errBreakerOpen) {
		t.Fatalf("mid-cooldown err = %v, want errBreakerOpen", err)
	}

	clock.t = clock.t.Add(BreakerCooldown)
	probed := false
	if err := b.do(func() error { probed = true; return nil }); err != nil {
		t.Fatalf("probe: %v, want success", err)
	}
	if !probed {
		t.Fatal("probe fn was not called after the cooldown")
	}
	// Closed again: calls flow and the failure count restarted from 0.
	for i := 0; i < breakerFailureThreshold-1; i++ {
		if err := b.do(func() error { return errRedisDown }); !errors.Is(err, errRedisDown) {
			t.Fatalf("post-close failure %d: err = %v, want the fn error", i+1, err)
		}
	}
	if err := b.do(func() error { return nil }); err != nil {
		t.Fatalf("still closed after threshold-1 failures: %v", err)
	}
}

func TestBreakerFailedProbeReopens(t *testing.T) {
	b, clock := newTestBreaker()
	trip(t, b)

	clock.t = clock.t.Add(BreakerCooldown)
	if err := b.do(func() error { return errRedisDown }); !errors.Is(err, errRedisDown) {
		t.Fatalf("probe err = %v, want the fn error", err)
	}
	// Fresh cooldown: rejected without advancing the clock.
	called := false
	if err := b.do(func() error { called = true; return nil }); !errors.Is(err, errBreakerOpen) {
		t.Fatalf("err = %v, want errBreakerOpen right after a failed probe", err)
	}
	if called {
		t.Fatal("fn called during the fresh cooldown")
	}
	// After another cooldown a new probe goes through.
	clock.t = clock.t.Add(BreakerCooldown)
	if err := b.do(func() error { return nil }); err != nil {
		t.Fatalf("second probe: %v, want success", err)
	}
}

func TestBreakerCanceledProbeAllowsImmediateReprobe(t *testing.T) {
	b, clock := newTestBreaker()
	trip(t, b)

	clock.t = clock.t.Add(BreakerCooldown)
	if err := b.do(func() error { return context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe: err = %v, want context.Canceled through", err)
	}
	// The canceled probe proved nothing: with openedAt untouched the
	// very next caller re-probes - no fresh cooldown, no wedged
	// half-open state.
	probed := false
	if err := b.do(func() error { probed = true; return nil }); err != nil {
		t.Fatalf("re-probe: %v, want success", err)
	}
	if !probed {
		t.Fatal("no immediate re-probe after a canceled probe")
	}
}

func TestBreakerSingleProbeDuringHalfOpen(t *testing.T) {
	b, clock := newTestBreaker()
	trip(t, b)
	clock.t = clock.t.Add(BreakerCooldown)

	probeEntered := make(chan struct{})
	release := make(chan struct{})
	probeErr := make(chan error, 1)
	go func() {
		probeErr <- b.do(func() error {
			close(probeEntered)
			<-release
			return nil
		})
	}()
	<-probeEntered

	// While the probe is in flight, everyone else is rejected without
	// touching redis.
	called := false
	if err := b.do(func() error { called = true; return nil }); !errors.Is(err, errBreakerOpen) {
		t.Fatalf("concurrent caller err = %v, want errBreakerOpen during the probe", err)
	}
	if called {
		t.Fatal("second fn ran concurrently with the half-open probe")
	}

	close(release)
	if err := <-probeErr; err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := b.do(func() error { return nil }); err != nil {
		t.Fatalf("after successful probe: %v, want closed circuit", err)
	}
}

func TestBreakerOpenTransitionLogsTheError(t *testing.T) {
	// Under degrade a permanently broken script (WRONGTYPE, bad Lua)
	// must not masquerade as a mere outage: the transition log has to
	// carry the underlying error.
	var buf bytes.Buffer
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	b := newBreaker(clock, slog.New(slog.NewTextHandler(&buf, nil)))
	scriptBug := errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	for i := 0; i < breakerFailureThreshold; i++ {
		_ = b.do(func() error { return scriptBug })
	}
	if !strings.Contains(buf.String(), "WRONGTYPE") {
		t.Errorf("open-transition log does not carry the underlying error:\n%s", buf.String())
	}
}

func TestBreakerRaceHammer(t *testing.T) {
	// Concurrency-correctness under -race: 100 goroutines through a
	// failing fn must leave the breaker open and consistent.
	b, _ := newTestBreaker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.do(func() error { return errRedisDown })
		}()
	}
	wg.Wait()

	called := false
	if err := b.do(func() error { called = true; return nil }); !errors.Is(err, errBreakerOpen) {
		t.Fatalf("err = %v, want errBreakerOpen after the hammer", err)
	}
	if called {
		t.Fatal("fn called while open after the hammer")
	}
}
