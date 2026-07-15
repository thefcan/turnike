package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

func TestRunGracefulShutdown(t *testing.T) {
	cfg, err := config.Parse([]byte(`
server:
  listen: "127.0.0.1:0"
  shutdown_timeout: 2s
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfg, slog.New(slog.DiscardHandler)) }()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned %v after cancel, want nil (graceful)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

// TestServeDrainsInFlightRequests proves shutdown actually drains: a
// request held in flight across the shutdown signal must complete with
// the gateway's rate-limit headers while new connections are refused.
// Swapping srv.Shutdown for srv.Close would fail this test — the held
// client would see a killed connection instead of its response.
func TestServeDrainsInFlightRequests(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("drained"))
	}))
	defer up.Close()

	// Generous shutdown_timeout: the test releases the upstream well
	// within it; the assertion is *that* draining happens, not how fast.
	cfg, err := config.Parse(fmt.Appendf(nil, `
server:
  listen: "127.0.0.1:0"
  shutdown_timeout: 5s
routes:
  - prefix: /
    upstream: %s
    limit: {algorithm: token_bucket, rate: 5, burst: 5}
`, up.URL))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, cfg, ln, slog.New(slog.DiscardHandler)) }()

	type result struct {
		status int
		header http.Header
		body   string
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/held")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		resCh <- result{status: resp.StatusCode, header: resp.Header, body: string(body)}
	}()

	// Only start shutting down once the request is provably in flight
	// (it reached the upstream through the gateway).
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("held request never reached the upstream")
	}
	cancel()

	// Shutdown closes the listener concurrently; poll until new
	// connections fail (any error counts — refused vs reset is
	// platform-dependent).
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepting new connections after shutdown began")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// With the doors closed, let the in-flight request finish and
	// assert it was served fully — status, body and the rate-limit
	// headers the gateway wrote before proxying.
	close(release)
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("in-flight request failed during drain: %v", res.err)
		}
		if res.status != http.StatusOK {
			t.Errorf("in-flight status = %d, want 200", res.status)
		}
		if res.body != "drained" {
			t.Errorf("in-flight body = %q, want %q", res.body, "drained")
		}
		if res.header.Get("X-RateLimit-Limit") == "" {
			t.Error("in-flight response lost its X-RateLimit-* headers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete during drain")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned %v after drain, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after draining")
	}
}
