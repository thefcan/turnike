package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/thefcan/turnike/internal/config"
	"github.com/thefcan/turnike/internal/limiter"
	"github.com/thefcan/turnike/internal/metrics"
)

// degradedLimiter is a Limiter double standing in for a RedisLimiter
// whose degrade fallback is answering: real verdicts, Degraded set.
type degradedLimiter struct{ allow bool }

func (d degradedLimiter) Allow(context.Context, string, config.Limit) (limiter.Decision, error) {
	return limiter.Decision{
		Allowed:    d.allow,
		Limit:      1,
		Remaining:  0,
		Reset:      time.Now().Add(time.Hour),
		RetryAfter: time.Second,
		Degraded:   true,
	}, nil
}

func TestDecisionLabel(t *testing.T) {
	tests := []struct {
		name string
		dec  limiter.Decision
		err  error
		want string
	}{
		{"backend allow", limiter.Decision{Allowed: true}, nil, metrics.DecisionAllow},
		{"backend deny", limiter.Decision{}, nil, metrics.DecisionDeny},
		{"fallback allow", limiter.Decision{Allowed: true, Degraded: true}, nil, metrics.DecisionDegradeAllow},
		{"fallback deny", limiter.Decision{Degraded: true}, nil, metrics.DecisionDegradeDeny},
		{"limiter error", limiter.Decision{}, context.DeadlineExceeded, metrics.DecisionDegrade},
	}
	for _, tt := range tests {
		if got := decisionLabel(tt.dec, tt.err); got != tt.want {
			t.Errorf("%s: decisionLabel = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// histCount reads the request-duration histogram's sample count.
func histCount(t *testing.T, m *metrics.Metrics) uint64 {
	t.Helper()
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() == "turnike_request_duration_seconds" {
			return f.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	t.Fatal("turnike_request_duration_seconds not gathered")
	return 0
}

// TestHandlerCountsAndObservesExactlyMatchedRequests drives the full
// handler chain and pins the central invariant: requests_total
// increments and histogram observations move in lockstep, on allowed
// AND denied requests, and on nothing else (404s, reserved paths).
func TestHandlerCountsAndObservesExactlyMatchedRequests(t *testing.T) {
	up := echoUpstream(t, "api")
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	m := metrics.New()
	cfg := &config.Config{
		Routes: []config.Route{{
			Prefix:   "/api/",
			Upstream: up.URL,
			Limit:    config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Minute)},
		}},
	}
	h, err := NewHandler(cfg, slog.New(slog.DiscardHandler), limiter.NewMemoryLimiter(clock), m)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	do := func(method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set(HeaderAPIKey, "demo")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	count := func(decision string) float64 {
		return testutil.ToFloat64(m.RequestsTotal.WithLabelValues("/api/", decision))
	}

	if got := do("GET", "/api/x"); got != http.StatusOK {
		t.Fatalf("1st request = %d, want 200", got)
	}
	if a, h := count(metrics.DecisionAllow), histCount(t, m); a != 1 || h != 1 {
		t.Errorf("after allow: counter=%v hist=%d, want 1/1", a, h)
	}

	// The denied request is counted AND observed - fast 429 exits are
	// real answers, not blind spots.
	if got := do("GET", "/api/x"); got != http.StatusTooManyRequests {
		t.Fatalf("2nd request = %d, want 429", got)
	}
	if d, h := count(metrics.DecisionDeny), histCount(t, m); d != 1 || h != 2 {
		t.Errorf("after deny: counter=%v hist=%d, want 1/2", d, h)
	}

	// Unrouted and reserved paths move neither: the route label comes
	// only from the config table, and a scrape must not count itself.
	if got := do("GET", "/nope"); got != http.StatusNotFound {
		t.Fatalf("/nope = %d, want 404", got)
	}
	if got := do("GET", "/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", got)
	}
	if h := histCount(t, m); h != 2 {
		t.Errorf("after 404 + /healthz: hist=%d, want still 2", h)
	}
	var total float64
	for _, d := range []string{metrics.DecisionAllow, metrics.DecisionDeny, metrics.DecisionDegrade, metrics.DecisionDegradeAllow, metrics.DecisionDegradeDeny} {
		total += count(d)
	}
	if total != 2 {
		t.Errorf("total counted = %v, want 2 (404s and reserved paths must not count)", total)
	}
}

// TestHandlerObservesFailClosed503 completes the observation invariant
// on the remaining outcome class: the fail_closed 503 is counted as
// bare degrade and observed by the histogram like any other answer.
func TestHandlerObservesFailClosed503(t *testing.T) {
	up := echoUpstream(t, "api")
	m := metrics.New()
	cfg := &config.Config{
		Limiter: config.Limiter{
			Backend: config.BackendRedis,
			Redis:   config.Redis{OnError: config.OnErrorFailClosed},
		},
		Routes: []config.Route{{Prefix: "/api/", Upstream: up.URL}},
	}
	h, err := NewHandler(cfg, slog.New(slog.DiscardHandler), erroringLimiter{}, m)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("/api/", metrics.DecisionDegrade)); got != 1 {
		t.Errorf("degrade count = %v, want 1", got)
	}
	if h := histCount(t, m); h != 1 {
		t.Errorf("hist count = %d, want 1 - the 503 answer has a duration too", h)
	}
}

// TestGatewayCountsDegradedVerdicts pins that the fallback's own
// allow/deny split survives into the label vocabulary, independent of
// the limiter package's internals.
func TestGatewayCountsDegradedVerdicts(t *testing.T) {
	up := echoUpstream(t, "api")
	for _, tt := range []struct {
		allow      bool
		wantStatus int
		wantLabel  string
	}{
		{true, http.StatusOK, metrics.DecisionDegradeAllow},
		{false, http.StatusTooManyRequests, metrics.DecisionDegradeDeny},
	} {
		m := metrics.New()
		g, err := NewGateway([]config.Route{{Prefix: "/api/", Upstream: up.URL}},
			config.Upstream{}, degradedLimiter{allow: tt.allow}, config.OnErrorDegrade, slog.New(slog.DiscardHandler), m)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/api/x", nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.wantStatus {
			t.Errorf("allow=%v: status = %d, want %d", tt.allow, w.Code, tt.wantStatus)
		}
		if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("/api/", tt.wantLabel)); got != 1 {
			t.Errorf("allow=%v: %s count = %v, want 1", tt.allow, tt.wantLabel, got)
		}
		// A degraded deny still carries the fallback's real headers.
		if !tt.allow {
			if got := w.Header().Get("Retry-After"); got == "" {
				t.Error("degraded 429 lost its Retry-After header")
			}
		}
	}
}

// TestRequestsTotalExposition is the canonical text-format pin: HELP,
// TYPE, label shape and pre-materialized zeros straight off the wire
// format Prometheus scrapes.
func TestRequestsTotalExposition(t *testing.T) {
	up := echoUpstream(t, "api")
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	m := metrics.New()
	g, err := NewGateway([]config.Route{{
		Prefix:   "/api/",
		Upstream: up.URL,
		Limit:    config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Minute)},
	}}, config.Upstream{}, limiter.NewMemoryLimiter(clock), config.OnErrorFailOpen, slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // one allow, one deny
		r := httptest.NewRequest("GET", "/api/x", nil)
		r.Header.Set(HeaderAPIKey, "demo")
		g.ServeHTTP(httptest.NewRecorder(), r)
	}

	const want = `
# HELP turnike_requests_total Requests that reached the rate-limit decision point, by configured route prefix and decision. allow/deny are the configured backend's own verdict; degrade_allow/degrade_deny are the degrade fallback's verdict; degrade means no verdict existed (fail_open pass-through or fail_closed 503). Unrouted and reserved paths are not counted.
# TYPE turnike_requests_total counter
turnike_requests_total{decision="allow",route="/api/"} 1
turnike_requests_total{decision="degrade",route="/api/"} 0
turnike_requests_total{decision="degrade_allow",route="/api/"} 0
turnike_requests_total{decision="degrade_deny",route="/api/"} 0
turnike_requests_total{decision="deny",route="/api/"} 1
`
	if err := testutil.CollectAndCompare(m.RequestsTotal, strings.NewReader(want)); err != nil {
		t.Errorf("exposition mismatch:\n%v", err)
	}
}
