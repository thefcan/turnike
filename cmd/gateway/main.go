// Command gateway is the turnike API gateway binary.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/thefcan/turnike/internal/config"
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

	// M0 skeleton: serve 501 on every route until the reverse proxy lands in M1.
	srv := &http.Server{
		Addr: cfg.Server.Listen,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "turnike: proxy not implemented yet (M1)", http.StatusNotImplemented)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("gateway listening", "addr", cfg.Server.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server", "err", err)
		os.Exit(1)
	}
}
