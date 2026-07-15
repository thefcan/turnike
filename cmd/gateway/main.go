// Command gateway is the turnike API gateway binary.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thefcan/turnike/internal/config"
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

// run serves the gateway until it fails or ctx is canceled, then drains
// in-flight requests for at most the configured shutdown timeout.
func run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	handler, err := proxy.NewHandler(cfg, logger) // M1: no ready checks yet
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeout),
		ReadTimeout:       time.Duration(cfg.Server.ReadTimeout),
		WriteTimeout:      time.Duration(cfg.Server.WriteTimeout),
		IdleTimeout:       time.Duration(cfg.Server.IdleTimeout),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", cfg.Server.Listen)
		errCh <- srv.ListenAndServe()
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
