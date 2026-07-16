package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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

// allowAllLimiter is a Limiter test double for tests that exercise
// routing/proxying and don't care about rate limiting: it never denies,
// so those tests stay decoupled from the algorithms' behavior. Tests
// that do care build a real *limiter.MemoryLimiter directly (see
// ratelimit_test.go).
type allowAllLimiter struct{}

func (allowAllLimiter) Allow(context.Context, string, config.Limit) (limiter.Decision, error) {
	return limiter.Decision{Allowed: true, Limit: 1, Remaining: 1, Reset: time.Now().Add(time.Hour)}, nil
}

// erroringLimiter is a Limiter test double for the fail-open path: every
// call errors, as if the backend were unreachable.
type erroringLimiter struct{}

func (erroringLimiter) Allow(context.Context, string, config.Limit) (limiter.Decision, error) {
	return limiter.Decision{}, errors.New("limiter backend unreachable")
}

// echoUpstream mirrors mock/main.go: it reports what it received so
// routing and header passthrough are observable from the test.
type echoPayload struct {
	Marker  string      `json:"marker"`
	Path    string      `json:"path"`
	Query   string      `json:"query"`
	Host    string      `json:"host"`
	Headers http.Header `json:"headers"`
}

func echoUpstream(t *testing.T, marker string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(echoPayload{
			Marker:  marker,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Host:    r.Host,
			Headers: r.Header,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestGateway(t *testing.T, routes []config.Route) *Gateway {
	t.Helper()
	g, err := NewGateway(routes, config.Upstream{}, allowAllLimiter{}, config.OnErrorFailOpen, slog.New(slog.DiscardHandler), metrics.New())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return g
}

func decodeEcho(t *testing.T, resp *http.Response) echoPayload {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var p echoPayload
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	return p
}

func TestGatewayRouting(t *testing.T) {
	api := echoUpstream(t, "api")
	search := echoUpstream(t, "search")
	g := newTestGateway(t, []config.Route{
		{Prefix: "/api/", Upstream: api.URL},
		{Prefix: "/search/", Upstream: search.URL},
	})
	front := httptest.NewServer(g)
	t.Cleanup(front.Close)

	tests := []struct {
		path       string
		wantMarker string
	}{
		{"/api/users?q=1", "api"},
		{"/search/x", "search"},
	}
	for _, tt := range tests {
		resp, err := http.Get(front.URL + tt.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tt.path, err)
		}
		p := decodeEcho(t, resp)
		if p.Marker != tt.wantMarker {
			t.Errorf("GET %s hit %q, want %q", tt.path, p.Marker, tt.wantMarker)
		}
		wantPath, wantQuery, _ := strings.Cut(tt.path, "?")
		if p.Path != wantPath || p.Query != wantQuery {
			t.Errorf("upstream saw path %q query %q, want %q %q", p.Path, p.Query, wantPath, wantQuery)
		}
	}
}

func TestGatewayHeaderPassthroughAndForwarded(t *testing.T) {
	up := echoUpstream(t, "api")
	g := newTestGateway(t, []config.Route{{Prefix: "/api/", Upstream: up.URL}})
	front := httptest.NewServer(g)
	t.Cleanup(front.Close)

	req, _ := http.NewRequest("GET", front.URL+"/api/x", nil)
	req.Header.Set("X-Custom", "hello")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	p := decodeEcho(t, resp)

	if got := p.Headers.Get("X-Custom"); got != "hello" {
		t.Errorf("X-Custom = %q, want passthrough of %q", got, "hello")
	}
	if got := p.Headers.Get("X-Forwarded-For"); got == "" {
		t.Error("X-Forwarded-For missing at upstream")
	}
	frontHost := strings.TrimPrefix(front.URL, "http://")
	if got := p.Headers.Get("X-Forwarded-Host"); got != frontHost {
		t.Errorf("X-Forwarded-Host = %q, want original host %q", got, frontHost)
	}
	if got := p.Headers.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http", got)
	}
	upstreamHost := strings.TrimPrefix(up.URL, "http://")
	if p.Host != upstreamHost {
		t.Errorf("upstream Host = %q, want rewritten to %q", p.Host, upstreamHost)
	}
}

func TestGatewayUnknownRoute(t *testing.T) {
	up := echoUpstream(t, "api")
	g := newTestGateway(t, []config.Route{{Prefix: "/api/", Upstream: up.URL}})

	for _, path := range []string{"/nope", "/apiv2"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
	}
}

func TestGatewayRejectsDotSegments(t *testing.T) {
	admin := echoUpstream(t, "admin")
	api := echoUpstream(t, "api")
	g := newTestGateway(t, []config.Route{
		{Prefix: "/api/", Upstream: api.URL},
		{Prefix: "/admin/", Upstream: admin.URL},
	})

	// Plain and percent-encoded dot segments must both 404: the matched
	// route and the path the upstream would resolve could diverge.
	for _, path := range []string{"/api/../admin/x", "/api/%2e%2e/admin/x", "/api/./x"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
	}
}

func TestGatewayResponseHeaderTimeoutIs502(t *testing.T) {
	// Distinct from TestGatewayDeadUpstream (a dial failure): here the
	// connection succeeds but the upstream sits on the response past the
	// configured header timeout, exercising the same errorHandler through
	// a different Transport failure.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	g, err := NewGateway(
		[]config.Route{{Prefix: "/api/", Upstream: slow.URL}},
		config.Upstream{ResponseHeaderTimeout: config.Duration(10 * time.Millisecond)},
		allowAllLimiter{}, config.OnErrorFailOpen, slog.New(slog.DiscardHandler), metrics.New())
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("response-header-timeout upstream = %d, want 502", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "bad gateway") {
		t.Errorf("body = %q, want bad gateway message", body)
	}
}

func TestGatewayFailsOpenOnLimiterError(t *testing.T) {
	// Under fail_open a limiter error must not take the upstream down
	// with it: the request still proceeds, just without X-RateLimit-*
	// (there's no Decision to report). The redis backend produces this
	// error for real when redis is down and the policy isn't degrade.
	up := echoUpstream(t, "api")
	m := metrics.New()
	g, err := NewGateway([]config.Route{{Prefix: "/api/", Upstream: up.URL}},
		config.Upstream{}, erroringLimiter{}, config.OnErrorFailOpen, slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (fail open despite the limiter error)", w.Code)
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "" {
		t.Errorf("X-RateLimit-Limit = %q, want absent — no Decision was returned to report", got)
	}
	// No verdict existed, so the pass-through counts as bare degrade.
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("/api/", metrics.DecisionDegrade)); got != 1 {
		t.Errorf("degrade count = %v, want 1 for the fail-open pass-through", got)
	}
}

func TestGatewayFailsClosedOnLimiterError(t *testing.T) {
	// Under fail_closed the same limiter error must reject instead:
	// 503 (not 429 - the client did nothing wrong), Retry-After from
	// the breaker cooldown, no X-RateLimit-* headers, upstream not hit.
	upstreamHit := false
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamHit = true
	}))
	t.Cleanup(up.Close)

	m := metrics.New()
	g, err := NewGateway([]config.Route{{Prefix: "/api/", Upstream: up.URL}},
		config.Upstream{}, erroringLimiter{}, config.OnErrorFailClosed, slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	// The 503 is the failure policy's answer, not a quota verdict: it
	// counts as bare degrade, not deny.
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("/api/", metrics.DecisionDegrade)); got != 1 {
		t.Errorf("degrade count = %v, want 1 for the fail-closed 503", got)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("/api/", metrics.DecisionDeny)); got != 0 {
		t.Errorf("deny count = %v, want 0 - a 503 is not a rate-limit deny", got)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q (the breaker cooldown, ceil'd)", got, "1")
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "" {
		t.Errorf("X-RateLimit-Limit = %q, want absent — there is no quota state to report", got)
	}
	if upstreamHit {
		t.Error("request was proxied despite fail_closed")
	}
}

func TestErrorHandlerRecordsClientCancelAs499(t *testing.T) {
	g := &Gateway{logger: slog.New(slog.DiscardHandler)}
	eh := g.errorHandler("http://example.invalid")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/api/x", nil).WithContext(ctx)
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}

	eh(rec, r, context.Canceled)

	if rec.status != statusClientClosedRequest {
		t.Errorf("recorder status = %d, want %d (client-closed)", rec.status, statusClientClosedRequest)
	}
	// Nothing should have been written to the underlying (dead)
	// connection — only the recorder's bookkeeping changes.
	if inner.Code != http.StatusOK {
		t.Errorf("wrote status %d to the dead connection, want no write at all", inner.Code)
	}
}

func TestGatewayDeadUpstream(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	_ = l.Close()

	g := newTestGateway(t, []config.Route{{Prefix: "/api/", Upstream: dead}})
	r := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("dead upstream = %d, want 502", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "bad gateway") {
		t.Errorf("body = %q, want bad gateway message", body)
	}
}
