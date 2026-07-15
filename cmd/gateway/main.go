// Command gateway is the turnike API gateway binary.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thefcan/turnike/internal/config"
	"github.com/thefcan/turnike/internal/limiter"
	"github.com/thefcan/turnike/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	logger.Info("config loaded",
		"listen", cfg.Server.Listen,
		"routes", len(cfg.Routes),
		"limiter_backend", cfg.Limiter.Backend,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, cfg, logger)
	stop()
	if err != nil {
		logger.Error("server", "err", err)
		os.Exit(1)
	}
}

// run binds cfg.Server.Listen and serves the gateway on it until it
// fails or ctx is canceled, then drains in-flight requests for at most
// the configured shutdown timeout.
func run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	return serve(ctx, cfg, ln, logger)
}

// serve runs the gateway on ln — which it owns and closes — until it
// fails or ctx is canceled, then drains in-flight requests for at most
// the configured shutdown timeout. Split from run so tests can bind
// their own listener and learn the real port.
func serve(ctx context.Context, cfg *config.Config, ln net.Listener, logger *slog.Logger) error {
	// Serve/Shutdown close ln themselves; closing again is a harmless
	// ErrClosed. This covers the error returns before Serve starts.
	defer func() { _ = ln.Close() }()

	lim, err := limiter.New(cfg.Limiter, limiter.RealClock{}, logger)
	if err != nil {
		return err
	}
	handler, err := proxy.NewHandler(cfg, logger, lim) // no ready checks until M3's Redis ping
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeout),
		ReadTimeout:       time.Duration(cfg.Server.ReadTimeout),
		WriteTimeout:      time.Duration(cfg.Server.WriteTimeout),
		IdleTimeout:       time.Duration(cfg.Server.IdleTimeout),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", ln.Addr().String())
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		timeout := time.Duration(cfg.Server.ShutdownTimeout)
		logger.Info("shutting down", "timeout", timeout)
		shCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return srv.Shutdown(shCtx)
	}
}
