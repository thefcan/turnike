package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/thefcan/turnike/internal/config"
	"github.com/thefcan/turnike/internal/limiter"
)

// manualClock is a limiter.Clock double so these integration tests can
// advance time deterministically, independent of internal/limiter's own
// (unexported) test clock.
type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time          { return c.t }
func (c *manualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newRateLimitedGateway(t *testing.T, routes []config.Route, clock *manualClock) (*Gateway, *httptest.Server) {
	t.Helper()
	g, err := NewGateway(routes, config.Upstream{}, limiter.NewMemoryLimiter(clock), config.OnErrorFailOpen, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	front := httptest.NewServer(g)
	t.Cleanup(front.Close)
	return g, front
}

func TestGatewayEnforcesRateLimit(t *testing.T) {
	up := echoUpstream(t, "api")
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 2, Window: config.Duration(time.Minute)}
	_, front := newRateLimitedGateway(t, []config.Route{{Prefix: "/api/", Upstream: up.URL, Limit: limit}}, clock)

	for i := 0; i < 2; i++ {
		resp, err := http.Get(front.URL + "/api/x")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
	}

	resp, err := http.Get(front.URL + "/api/x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd request: status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if got := resp.Header.Get("X-RateLimit-Limit"); got != "2" {
		t.Errorf("X-RateLimit-Limit = %q, want 2", got)
	}
	retryAfter, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || retryAfter < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", resp.Header.Get("Retry-After"))
	}
}

func TestGatewayRateLimitHeadersSurviveProxying(t *testing.T) {
	up := echoUpstream(t, "api")
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 5, Window: config.Duration(time.Minute)}
	_, front := newRateLimitedGateway(t, []config.Route{{Prefix: "/api/", Upstream: up.URL, Limit: limit}}, clock)

	resp, err := http.Get(front.URL + "/api/x")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Limit"); got != "5" {
		t.Errorf("X-RateLimit-Limit = %q, want 5", got)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "4" {
		t.Errorf("X-RateLimit-Remaining = %q, want 4", got)
	}
	if resp.Header.Get("X-RateLimit-Reset") == "" {
		t.Error("X-RateLimit-Reset missing")
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Error("Retry-After present on an allowed response, want absent")
	}
	// The upstream's own response body must have come through: proves
	// ReverseProxy's additive header copy didn't come at the cost of
	// actually proxying the request.
	p := decodeEcho(t, resp)
	if p.Marker != "api" {
		t.Errorf("upstream marker = %q, want api — request wasn't actually proxied", p.Marker)
	}
}

func TestGatewayRateLimitPerIdentityIsolation(t *testing.T) {
	up := echoUpstream(t, "api")
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Minute)}
	g, _ := newRateLimitedGateway(t, []config.Route{{Prefix: "/api/", Upstream: up.URL, Limit: limit}}, clock)

	get := func(apiKey string) int {
		r := httptest.NewRequest("GET", "/api/x", nil)
		if apiKey != "" {
			r.Header.Set(HeaderAPIKey, apiKey)
		} else {
			r.RemoteAddr = "9.9.9.9:1234"
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}

	if got := get("alice"); got != http.StatusOK {
		t.Errorf("alice's 1st request = %d, want 200", got)
	}
	if got := get("alice"); got != http.StatusTooManyRequests {
		t.Errorf("alice's 2nd request = %d, want 429", got)
	}
	// A different identity must have its own, untouched quota.
	if got := get("bob"); got != http.StatusOK {
		t.Errorf("bob's 1st request = %d, want 200 (independent from alice's quota)", got)
	}
	if got := get(""); got != http.StatusOK {
		t.Errorf("anonymous (IP-keyed) 1st request = %d, want 200", got)
	}
}

func TestGatewayRateLimitKeyOverride(t *testing.T) {
	up := echoUpstream(t, "api")
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	route := config.Route{
		Prefix:   "/api/",
		Upstream: up.URL,
		Limit:    config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Minute)},
		KeyOverrides: map[string]config.Limit{
			"vip": {Rate: 3},
		},
	}
	g, _ := newRateLimitedGateway(t, []config.Route{route}, clock)

	get := func(apiKey string) int {
		r := httptest.NewRequest("GET", "/api/x", nil)
		r.Header.Set(HeaderAPIKey, apiKey)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}

	// The base limit (rate 1) denies a plain key's 2nd request...
	if got := get("plain"); got != http.StatusOK {
		t.Fatalf("plain's 1st request = %d, want 200", got)
	}
	if got := get("plain"); got != http.StatusTooManyRequests {
		t.Fatalf("plain's 2nd request = %d, want 429", got)
	}
	// ...but the "vip" override (rate 3) allows 3 before denying.
	for i := 0; i < 3; i++ {
		if got := get("vip"); got != http.StatusOK {
			t.Fatalf("vip's request %d = %d, want 200 (override rate 3)", i, got)
		}
	}
	if got := get("vip"); got != http.StatusTooManyRequests {
		t.Fatalf("vip's 4th request = %d, want 429", got)
	}
}

func TestGatewayRateLimitRecoversAfterWindow(t *testing.T) {
	up := echoUpstream(t, "api")
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(10 * time.Second)}
	g, _ := newRateLimitedGateway(t, []config.Route{{Prefix: "/api/", Upstream: up.URL, Limit: limit}}, clock)

	do := func() int {
		r := httptest.NewRequest("GET", "/api/x", nil)
		r.Header.Set(HeaderAPIKey, "demo")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}

	if got := do(); got != http.StatusOK {
		t.Fatalf("1st request = %d, want 200", got)
	}
	if got := do(); got != http.StatusTooManyRequests {
		t.Fatalf("2nd request (same window) = %d, want 429", got)
	}
	clock.Advance(10 * time.Second)
	if got := do(); got != http.StatusOK {
		t.Fatalf("request after the window rolled over = %d, want 200", got)
	}
}

func TestGatewayRateLimitNamespacedByRoute(t *testing.T) {
	api := echoUpstream(t, "api")
	search := echoUpstream(t, "search")
	clock := &manualClock{t: time.Unix(1_000_000_000, 0)}
	limit := config.Limit{Algorithm: config.AlgoFixedWindow, Rate: 1, Window: config.Duration(time.Minute)}
	g, _ := newRateLimitedGateway(t, []config.Route{
		{Prefix: "/api/", Upstream: api.URL, Limit: limit},
		{Prefix: "/search/", Upstream: search.URL, Limit: limit},
	}, clock)

	do := func(path string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set(HeaderAPIKey, "demo")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}

	// The same identity exhausts /api/'s quota...
	if got := do("/api/x"); got != http.StatusOK {
		t.Fatalf("/api/x 1st request = %d, want 200", got)
	}
	if got := do("/api/x"); got != http.StatusTooManyRequests {
		t.Fatalf("/api/x 2nd request = %d, want 429", got)
	}
	// ...but /search/ has its own independent quota for that identity.
	if got := do("/search/x"); got != http.StatusOK {
		t.Fatalf("/search/x 1st request = %d, want 200 (independent quota from /api/)", got)
	}
}
