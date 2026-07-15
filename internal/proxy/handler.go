package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/thefcan/turnike/internal/config"
	"github.com/thefcan/turnike/internal/limiter"
)

// ReadyCheck reports whether a dependency is ready to serve traffic; a
// non-nil error turns /readyz into a 503. The redis backend's ping
// plugs in here - but only under fail_closed (see cmd/gateway/main.go
// for the policy reasoning).
type ReadyCheck func(context.Context) error

// readyCheckTimeout bounds each ReadyCheck so a hung dependency turns
// into a fast 503 instead of a probe pinned until the prober gives up.
// A dependency that cannot answer within a second is not ready by any
// useful definition; a knob for this would be pure config surface.
const readyCheckTimeout = time.Second

// NewHandler wires the health endpoints, the request-ID/access-log
// middleware and the gateway (with the given Limiter) into the root
// handler. /healthz and /readyz are reserved: they take precedence over
// configured routes and bypass the middleware.
//
// lim is injected rather than built from cfg here so tests can drive a
// MemoryLimiter on a manual clock; cmd/gateway/main.go builds the
// production one via limiter.New(cfg.Limiter, limiter.RealClock{}).
//
// The dispatch is deliberately not an http.ServeMux — the mux
// 301-redirects uncleaned paths, which would break POSTs through the
// gateway; path cleaning belongs to the route table alone.
func NewHandler(cfg *config.Config, logger *slog.Logger, lim limiter.Limiter, ready ...ReadyCheck) (http.Handler, error) {
	gw, err := NewGateway(cfg.Routes, cfg.Upstream, lim, cfg.Limiter.Redis.OnError, logger)
	if err != nil {
		return nil, err
	}
	proxied := Middleware(logger)(gw)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			// Probes speak GET/HEAD; answering 200 to anything else
			// would let a misconfigured monitor read healthy forever.
			// The proxy branch below stays method-agnostic - the
			// gateway must forward every method. (net/http suppresses
			// response bodies on HEAD by itself.)
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeText(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			if r.URL.Path == "/healthz" {
				writeText(w, http.StatusOK, "ok")
				return
			}
			for _, check := range ready {
				cctx, cancel := context.WithTimeout(r.Context(), readyCheckTimeout)
				err := check(cctx)
				cancel()
				if err != nil {
					// Reason to the log, not the body: these endpoints
					// answer ahead of the route table to any caller,
					// and dependency errors name internal topology
					// (e.g. the redis addr).
					logger.Warn("readiness check failed", "err", err)
					writeText(w, http.StatusServiceUnavailable, "not ready")
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
