package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thefcan/turnike/internal/config"
	"github.com/thefcan/turnike/internal/metrics"
)

func newTestHandler(t *testing.T, logger *slog.Logger, upstream string, ready ...ReadyCheck) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Routes: []config.Route{{Prefix: "/", Upstream: upstream}},
	}
	h, err := NewHandler(cfg, logger, allowAllLimiter{}, metrics.New(), ready...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func TestHandlerHealthEndpoints(t *testing.T) {
	up := echoUpstream(t, "api")
	h := newTestHandler(t, slog.New(slog.DiscardHandler), up.URL)

	// The route table has a catch-all "/" route, yet the reserved health
	// paths must still be answered by the gateway itself.
	for _, path := range []string{"/healthz", "/readyz"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
		}
		if body := w.Body.String(); !strings.Contains(body, "ok") {
			t.Errorf("GET %s body = %q, want ok", path, body)
		}
		if got := w.Header().Get("X-Upstream"); got != "" {
			t.Errorf("GET %s was proxied to upstream (X-Upstream %q)", path, got)
		}
	}
}

func TestHandlerReadyCheckFailure(t *testing.T) {
	up := echoUpstream(t, "api")
	var buf bytes.Buffer
	h := newTestHandler(t, slog.New(slog.NewTextHandler(&buf, nil)), up.URL,
		func(context.Context) error { return nil },
		func(context.Context) error { return errors.New("dial tcp 10.0.0.5:6379: connect: connection refused") },
	)

	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503", w.Code)
	}
	// The reason goes to the log; the body stays generic - these
	// endpoints answer any caller ahead of the route table, and the
	// error names internal topology.
	if body := w.Body.String(); strings.Contains(body, "10.0.0.5") {
		t.Errorf("/readyz body leaks the dependency address: %q", body)
	}
	if !strings.Contains(buf.String(), "10.0.0.5") {
		t.Errorf("readiness failure reason missing from the log:\n%s", buf.String())
	}
}

func TestHandlerMemoryBackendIgnoresOnError(t *testing.T) {
	// A leftover redis.on_error: fail_closed in a memory-backend config
	// must not turn the memory limiter's at-capacity error into a 503 -
	// that error fails open by design, and there is no breaker whose
	// cooldown the 503's Retry-After would describe.
	up := echoUpstream(t, "api")
	cfg := &config.Config{
		Limiter: config.Limiter{
			Backend: config.BackendMemory,
			Redis:   config.Redis{OnError: config.OnErrorFailClosed},
		},
		Routes: []config.Route{{Prefix: "/", Upstream: up.URL}},
	}
	h, err := NewHandler(cfg, slog.New(slog.DiscardHandler), erroringLimiter{}, metrics.New())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (memory backend always fails open)", w.Code)
	}
}

func TestHandlerHealthMethodGuard(t *testing.T) {
	up := echoUpstream(t, "api")
	h := newTestHandler(t, slog.New(slog.DiscardHandler), up.URL)

	// A monitor probing with the wrong method must not read healthy -
	// and promhttp on its own would answer a POST /metrics.
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			r := httptest.NewRequest(method, path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, path, w.Code)
			}
			if got := w.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("%s %s Allow = %q, want %q", method, path, got, "GET, HEAD")
			}
		}
		// HEAD is a probe verb and must keep working.
		r := httptest.NewRequest("HEAD", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", path, w.Code)
		}
	}

	// The proxy branch stays method-agnostic: a POST to a routed path
	// must still be forwarded, not 405'd.
	r := httptest.NewRequest("POST", "/api/things", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("POST /api/things = %d, want 200 (proxied)", w.Code)
	}
}

func TestHandlerReadyCheckTimeout(t *testing.T) {
	up := echoUpstream(t, "api")
	h := newTestHandler(t, slog.New(slog.DiscardHandler), up.URL,
		// A hung dependency: blocks until the per-check budget cancels it.
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	)

	start := time.Now()
	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	elapsed := time.Since(start)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 from the timed-out check", w.Code)
	}
	// The budget is 1s; well under 5s proves the probe wasn't pinned
	// for the prober's lifetime.
	if elapsed > 5*time.Second {
		t.Errorf("/readyz took %v, want ~%v (per-check timeout)", elapsed, readyCheckTimeout)
	}
}

func TestHandlerMetricsEndpoint(t *testing.T) {
	up := echoUpstream(t, "api")
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	m := metrics.New()
	cfg := &config.Config{Routes: []config.Route{{Prefix: "/", Upstream: up.URL}}}
	h, err := NewHandler(cfg, logger, allowAllLimiter{}, m)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	// One routed request so the scrape has something to show.
	r := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("routed request = %d, want 200", w.Code)
	}

	scrape := func() *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		return w
	}
	w = scrape()
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, family := range []string{
		"turnike_requests_total",
		"turnike_request_duration_seconds",
		"turnike_breaker_state",
		"turnike_limiter_backend",
	} {
		if !strings.Contains(body, family) {
			t.Errorf("scrape is missing family %s", family)
		}
	}
	const oneAllow = `turnike_requests_total{decision="allow",route="/"} 1`
	if !strings.Contains(body, oneAllow) {
		t.Errorf("scrape does not show the routed request:\n%s", body)
	}
	if strings.Contains(body, "go_goroutines") {
		t.Error("scrape leaks Go runtime collectors; the registry must stay at the four families")
	}

	// Scraping is self-invisible: reserved paths bypass the middleware,
	// so a scrape is neither counted, nor observed, nor access-logged.
	w = scrape()
	if !strings.Contains(w.Body.String(), oneAllow) {
		t.Error("a scrape moved the counters; /metrics must not count itself")
	}
	if n := strings.Count(buf.String(), `"msg":"request"`); n != 1 {
		t.Errorf("access log has %d request lines, want 1 (scrapes must not log)", n)
	}
}

func TestHandlerFullChain(t *testing.T) {
	up := echoUpstream(t, "api")
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := newTestHandler(t, logger, up.URL)
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	// Health first: it must not leave an access-log line.
	resp, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if buf.Len() != 0 {
		t.Errorf("/healthz produced a log line: %s", buf.String())
	}

	req, _ := http.NewRequest("GET", front.URL+"/api/users", nil)
	req.Header.Set(HeaderAPIKey, "demo")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	p := decodeEcho(t, resp)

	respID := resp.Header.Get(HeaderRequestID)
	if !hexID.MatchString(respID) {
		t.Errorf("response %s = %q, want generated 32-hex", HeaderRequestID, respID)
	}
	if got := p.Headers.Get(HeaderRequestID); got != respID {
		t.Errorf("upstream saw %s %q, response carries %q; want equal", HeaderRequestID, got, respID)
	}

	var line struct {
		Msg       string `json:"msg"`
		Status    int    `json:"status"`
		Route     string `json:"route"`
		Identity  string `json:"identity"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("expected exactly one access-log line: %v\n%s", err, buf.String())
	}
	if line.Msg != "request" || line.Status != http.StatusOK {
		t.Errorf("unexpected access log: %+v", line)
	}
	if line.Route != "/" {
		t.Errorf("route = %q, want /", line.Route)
	}
	if want := keyFingerprint("demo"); line.Identity != want {
		t.Errorf("identity = %q, want fingerprint %q", line.Identity, want)
	}
	if line.RequestID != respID {
		t.Errorf("logged request_id = %q, response id = %q; want equal", line.RequestID, respID)
	}
}
