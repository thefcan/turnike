package limiter

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/thefcan/turnike/internal/config"
)

// fakeScripter is a redis.Scripter double so the glue around the Lua
// scripts - key scheme, ARGV building, reply mapping, error paths - is
// unit-testable without a redis. The scripts' own behavior is covered
// against a real redis in redis_integration_test.go.
type fakeScripter struct {
	mu           sync.Mutex
	reply        any   // Eval/EvalSha reply value when no error applies
	err          error // returned by both Eval and EvalSha
	evalShaErr   error // returned by EvalSha only (e.g. a NOSCRIPT)
	evalCalls    int
	evalShaCalls int
	lastKeys     []string
	lastArgs     []any
}

func (f *fakeScripter) Eval(ctx context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evalCalls++
	f.lastKeys, f.lastArgs = keys, args
	cmd := redis.NewCmd(ctx)
	if f.err != nil {
		cmd.SetErr(f.err)
	} else {
		cmd.SetVal(f.reply)
	}
	return cmd
}

func (f *fakeScripter) EvalSha(ctx context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evalShaCalls++
	f.lastKeys, f.lastArgs = keys, args
	cmd := redis.NewCmd(ctx)
	switch {
	case f.evalShaErr != nil:
		cmd.SetErr(f.evalShaErr)
	case f.err != nil:
		cmd.SetErr(f.err)
	default:
		cmd.SetVal(f.reply)
	}
	return cmd
}

func (f *fakeScripter) EvalRO(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	return f.Eval(ctx, script, keys, args...)
}

func (f *fakeScripter) EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...any) *redis.Cmd {
	return f.EvalSha(ctx, sha1, keys, args...)
}

func (f *fakeScripter) ScriptExists(ctx context.Context, hashes ...string) *redis.BoolSliceCmd {
	cmd := redis.NewBoolSliceCmd(ctx)
	cmd.SetVal(make([]bool, len(hashes)))
	return cmd
}

func (f *fakeScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	cmd.SetVal("")
	return cmd
}

func newFakeRedisLimiter(f *fakeScripter) *RedisLimiter {
	logger := slog.New(slog.DiscardHandler)
	return &RedisLimiter{
		scripter: f,
		breaker:  newBreaker(&manualClock{t: time.Unix(1_000_000_000, 0)}, logger),
		logger:   logger,
	}
}

func TestRedisLimiterKeySchemeAndArgs(t *testing.T) {
	const resetUS = int64(1_700_000_060_000_000)
	f := &fakeScripter{reply: []any{int64(1), int64(4), resetUS, int64(0)}}
	l := newFakeRedisLimiter(f)
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 5, Window: config.Duration(10 * time.Second)}

	dec, err := l.Allow(context.Background(), "/api:key:ab12cd34", limit)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}

	// The settled scheme: turnike:{algo}:{gateway key}, exactly the
	// memory backend's state key under the turnike: prefix.
	wantKey := "turnike:fixed_window:/api:key:ab12cd34"
	if len(f.lastKeys) != 1 || f.lastKeys[0] != wantKey {
		t.Errorf("KEYS = %v, want [%q]", f.lastKeys, wantKey)
	}
	wantArgs := []any{5, int64(10_000_000)} // rate, window in µs
	if !reflect.DeepEqual(f.lastArgs, wantArgs) {
		t.Errorf("ARGV = %#v, want %#v", f.lastArgs, wantArgs)
	}
	want := Decision{Allowed: true, Limit: 5, Remaining: 4, Reset: time.UnixMicro(resetUS)}
	if !reflect.DeepEqual(dec, want) {
		t.Errorf("Decision = %+v, want %+v", dec, want)
	}
}

func TestRedisLimiterDeniedDecision(t *testing.T) {
	const resetUS = int64(1_700_000_060_000_000)
	const retryUS = int64(2_500_000)
	f := &fakeScripter{reply: []any{int64(0), int64(0), resetUS, retryUS}}
	l := newFakeRedisLimiter(f)
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 5, Window: config.Duration(10 * time.Second)}

	dec, err := l.Allow(context.Background(), "k", limit)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if dec.Allowed {
		t.Error("Allowed = true, want denied")
	}
	if want := 2500 * time.Millisecond; dec.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v", dec.RetryAfter, want)
	}
	if !dec.Reset.Equal(time.UnixMicro(resetUS)) {
		t.Errorf("Reset = %v, want %v", dec.Reset, time.UnixMicro(resetUS))
	}
	if dec.Degraded {
		t.Error("Degraded = true on redis's own verdict")
	}
}

func TestRedisLimiterNoScriptFallsBackToEval(t *testing.T) {
	// A redis restart or SCRIPT FLUSH empties the script cache; the very
	// next decision must self-heal via the EVAL fallback, not error.
	// Script.Run matches the sentinel with errors.Is, so the fake must
	// surface redis.ErrNoScript itself - a plain errors.New would not
	// exercise the fallback.
	f := &fakeScripter{
		evalShaErr: redis.ErrNoScript,
		reply:      []any{int64(1), int64(0), int64(1), int64(0)},
	}
	l := newFakeRedisLimiter(f)
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Second)}

	dec, err := l.Allow(context.Background(), "k", limit)
	if err != nil {
		t.Fatalf("Allow after NOSCRIPT: %v, want the EVAL fallback to succeed", err)
	}
	if !dec.Allowed {
		t.Error("Allowed = false, want true from the EVAL fallback")
	}
	if f.evalShaCalls != 1 || f.evalCalls != 1 {
		t.Errorf("calls = %d EVALSHA / %d EVAL, want 1/1", f.evalShaCalls, f.evalCalls)
	}
}

func TestRedisLimiterErrorPropagates(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	f := &fakeScripter{err: sentinel}
	l := newFakeRedisLimiter(f)
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Second)}

	_, err := l.Allow(context.Background(), "k", limit)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
}

func TestRedisLimiterMalformedReply(t *testing.T) {
	f := &fakeScripter{reply: []any{int64(1), int64(2)}}
	l := newFakeRedisLimiter(f)
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Second)}

	_, err := l.Allow(context.Background(), "k", limit)
	if err == nil || !strings.Contains(err.Error(), "want 4") {
		t.Fatalf("err = %v, want a malformed-reply error", err)
	}
}

func TestRedisLimiterSubMicrosecondWindow(t *testing.T) {
	f := &fakeScripter{}
	l := newFakeRedisLimiter(f)
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(500)} // 500ns

	_, err := l.Allow(context.Background(), "k", limit)
	if err == nil || !strings.Contains(err.Error(), "1µs resolution") {
		t.Fatalf("err = %v, want the resolution guard", err)
	}
	if f.evalShaCalls != 0 || f.evalCalls != 0 {
		t.Error("script was called despite the resolution guard")
	}
}

func TestRedisLimiterUnknownAlgorithm(t *testing.T) {
	l := newFakeRedisLimiter(&fakeScripter{})
	limit := config.Limit{Algorithm: "other_algo", Rate: 1, Window: config.Duration(time.Second)}

	_, err := l.Allow(context.Background(), "k", limit)
	if err == nil || !strings.Contains(err.Error(), "no redis script") {
		t.Fatalf("err = %v, want the unknown-algorithm error", err)
	}
}

func TestRedisLimiterDegradeFallsBackToMemory(t *testing.T) {
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	logger := slog.New(slog.DiscardHandler)
	f := &fakeScripter{err: errors.New("dial tcp: connection refused")}
	l := &RedisLimiter{
		scripter: f,
		breaker:  newBreaker(clock, logger),
		fallback: NewMemoryLimiter(clock),
		logger:   logger,
	}
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 2, Window: config.Duration(time.Minute)}

	// A fresh in-memory bucket answers: allowed with Remaining = rate-1,
	// which the erroring fake could never have produced.
	dec, err := l.Allow(context.Background(), "k", limit)
	if err != nil {
		t.Fatalf("Allow under degrade: %v, want the fallback to answer", err)
	}
	if !dec.Allowed || dec.Remaining != 1 {
		t.Errorf("Decision = %+v, want allowed with Remaining 1 from a fresh memory bucket", dec)
	}
	if !dec.Degraded {
		t.Error("Degraded = false on a fallback answer; the caller cannot tell degrade happened")
	}
	// And the fallback enforces: the instance-local quota still denies.
	if dec, _ := l.Allow(context.Background(), "k", limit); !dec.Allowed || !dec.Degraded {
		t.Fatalf("second request: %+v, want a degraded allow (rate 2)", dec)
	}
	// The denied fallback answer is degraded too - a redis outage must
	// not erase 429s from the requests_total decision label.
	if dec, _ := l.Allow(context.Background(), "k", limit); dec.Allowed || !dec.Degraded {
		t.Errorf("third request: %+v, want a degraded deny from the fallback", dec)
	}
}

func TestRedisLimiterBreakerStopsCallingRedis(t *testing.T) {
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	logger := slog.New(slog.DiscardHandler)
	f := &fakeScripter{err: errors.New("dial tcp: connection refused")}
	l := &RedisLimiter{
		scripter: f,
		breaker:  newBreaker(clock, logger),
		fallback: NewMemoryLimiter(clock),
		logger:   logger,
	}
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 100, Window: config.Duration(time.Minute)}

	for i := 0; i < 10; i++ {
		dec, err := l.Allow(context.Background(), "k", limit)
		if err != nil {
			t.Fatalf("Allow %d: %v, want the fallback to absorb the failure", i+1, err)
		}
		if !dec.Allowed {
			t.Fatalf("Allow %d: denied at rate 100", i+1)
		}
	}
	// The breaker tripped at the threshold: redis stopped being dialed
	// even though all 10 requests were answered (by the fallback).
	if f.evalShaCalls != breakerFailureThreshold {
		t.Errorf("redis saw %d calls over 10 requests, want exactly %d (breaker must short-circuit the rest)",
			f.evalShaCalls, breakerFailureThreshold)
	}
}
