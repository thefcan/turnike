package main

import (
	"context"
	"log/slog"
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
