// Command mock is a tiny echo upstream used by the dev compose file and the
// multi-instance demo: it answers every request with a JSON dump of what it
// received, so proxy behaviour (routing, header passthrough) is observable
// from the outside.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"
)

type echo struct {
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Query   string      `json:"query,omitempty"`
	Headers http.Header `json:"headers"`
}

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "mock")
		if err := json.NewEncoder(w).Encode(echo{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Headers: r.Header,
		}); err != nil {
			logger.Error("encode response", "err", err)
		}
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("mock upstream listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server", "err", err)
		os.Exit(1)
	}
}
