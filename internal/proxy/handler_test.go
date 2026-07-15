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

	"github.com/thefcan/turnike/internal/config"
)

func newTestHandler(t *testing.T, logger *slog.Logger, upstream string, ready ...ReadyCheck) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Routes: []config.Route{{Prefix: "/", Upstream: upstream}},
	}
	h, err := NewHandler(cfg, logger, ready...)
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
	h := newTestHandler(t, slog.New(slog.DiscardHandler), up.URL,
		func(context.Context) error { return nil },
		func(context.Context) error { return errors.New("redis is down") },
	)

	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "redis is down") {
		t.Errorf("/readyz body = %q, want the check's reason", body)
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
	if line.Identity != "key:demo" {
		t.Errorf("identity = %q, want key:demo", line.Identity)
	}
	if line.RequestID != respID {
		t.Errorf("logged request_id = %q, response id = %q; want equal", line.RequestID, respID)
	}
}
