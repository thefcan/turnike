package proxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/thefcan/turnike/internal/config"
)

// ReadyCheck reports whether a dependency is ready to serve traffic; a
// non-nil error turns /readyz into a 503. The redis backend plugs in a
// ping here in M3.
type ReadyCheck func(context.Context) error

// NewHandler wires the health endpoints, the request-ID/access-log
// middleware and the gateway into the root handler. /healthz and /readyz
// are reserved: they take precedence over configured routes and bypass
// the middleware.
//
// The dispatch is deliberately not an http.ServeMux — the mux
// 301-redirects uncleaned paths, which would break POSTs through the
// gateway; path cleaning belongs to the route table alone.
func NewHandler(cfg *config.Config, logger *slog.Logger, ready ...ReadyCheck) (http.Handler, error) {
	gw, err := NewGateway(cfg.Routes, cfg.Upstream, logger)
	if err != nil {
		return nil, err
	}
	proxied := Middleware(logger)(gw)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			writeText(w, http.StatusOK, "ok")
		case "/readyz":
			for _, check := range ready {
				if err := check(r.Context()); err != nil {
					writeText(w, http.StatusServiceUnavailable, "not ready: "+err.Error())
					return
				}
			}
			writeText(w, http.StatusOK, "ok")
		default:
			proxied.ServeHTTP(w, r)
		}
	}), nil
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}
