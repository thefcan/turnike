package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/thefcan/turnike/internal/config"
	"github.com/thefcan/turnike/internal/limiter"
	"github.com/thefcan/turnike/internal/metrics"
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

// NewHandler wires the health endpoints, the metrics endpoint, the
// request-ID/access-log middleware and the gateway (with the given
// Limiter) into the root handler. /healthz, /readyz and /metrics are
// reserved: they take precedence over configured routes and bypass the
// middleware, so a scrape is never logged, rate-limited or counted.
//
// lim is injected rather than built from cfg here so tests can drive a
// MemoryLimiter on a manual clock; cmd/gateway/main.go builds the
// production one via limiter.New with its Instruments wired.
//
// The dispatch is deliberately not an http.ServeMux — the mux
// 301-redirects uncleaned paths, which would break POSTs through the
// gateway; path cleaning belongs to the route table alone.
func NewHandler(cfg *config.Config, logger *slog.Logger, lim limiter.Limiter, m *metrics.Metrics, ready ...ReadyCheck) (http.Handler, error) {
	// The on_error policy governs *redis* failures only. Under the
	// memory backend the sole runtime error is the at-capacity guard,
	// which fails open by design - a leftover redis.on_error line in a
	// memory config must not turn that into a 503.
	onError := config.OnErrorFailOpen
	if cfg.Limiter.Backend == config.BackendRedis {
		onError = cfg.Limiter.Redis.OnError
	}
	gw, err := NewGateway(cfg.Routes, cfg.Upstream, lim, onError, logger, m)
	if err != nil {
		return nil, err
	}
	proxied := Middleware(logger, m)(gw)
	metricsHandler := m.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics":
			// Probes and scrapers speak GET/HEAD; answering 200 to
			// anything else would let a misconfigured monitor read
			// healthy forever - and promhttp left alone would answer a
			// POST with metrics, so the guard is load-bearing here, not
			// cosmetic. The proxy branch below stays method-agnostic -
			// the gateway must forward every method. (net/http
			// suppresses response bodies on HEAD by itself.)
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeText(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			switch r.URL.Path {
			case "/metrics":
				metricsHandler.ServeHTTP(w, r)
			case "/healthz":
				writeText(w, http.StatusOK, "ok")
			default: // /readyz
				for _, check := range ready {
					cctx, cancel := context.WithTimeout(r.Context(), readyCheckTimeout)
					err := check(cctx)
					cancel()
					if err != nil {
						// Reason to the log, not the body: these
						// endpoints answer ahead of the route table to
						// any caller, and dependency errors name
						// internal topology (e.g. the redis addr).
						logger.Warn("readiness check failed", "err", err)
						writeText(w, http.StatusServiceUnavailable, "not ready")
						return
					}
				}
				writeText(w, http.StatusOK, "ok")
			}
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
