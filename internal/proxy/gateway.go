package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thefcan/turnike/internal/config"
	"github.com/thefcan/turnike/internal/limiter"
)

// Gateway routes requests to their upstream via one reverse proxy per
// route, after a per-route, per-identity rate-limit check. Unmatched
// paths get a 404, denied requests a 429, upstream failures a 502.
type Gateway struct {
	table   *Table
	proxies map[string]*httputil.ReverseProxy // keyed by normalized Entry.Prefix
	limiter limiter.Limiter
	logger  *slog.Logger
}

// NewGateway compiles routes into a gateway. All routes share one
// transport (and thus one connection pool) with the given timeouts, and
// the given Limiter for rate-limit decisions.
func NewGateway(routes []config.Route, up config.Upstream, lim limiter.Limiter, logger *slog.Logger) (*Gateway, error) {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: time.Duration(up.DialTimeout)}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: time.Duration(up.ResponseHeaderTimeout),
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   32,
		ForceAttemptHTTP2:     true,
	}
	g := &Gateway{
		table:   NewTable(routes),
		proxies: make(map[string]*httputil.ReverseProxy, len(routes)),
		limiter: lim,
		logger:  logger,
	}
	for _, e := range g.table.entries {
		target, err := url.Parse(e.Route.Upstream)
		if err != nil {
			// Unreachable after config validation, but don't proxy blind.
			return nil, fmt.Errorf("route %q: upstream %q: %w", e.Route.Prefix, e.Route.Upstream, err)
		}
		g.proxies[e.Prefix] = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target) // path forwarded unchanged; Host becomes the upstream host
				pr.SetXForwarded()
			},
			Transport:    transport,
			ErrorHandler: g.errorHandler(e.Route.Upstream),
			ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
		}
	}
	return g, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Dot segments are rejected outright: matching runs on the cleaned
	// path but the URL is forwarded unchanged, so letting ".." through —
	// plain or percent-encoded, which only surfaces after decoding —
	// would let the matched route and the path the upstream resolves
	// diverge.
	if hasDotSegments(r.URL.Path) {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	entry, ok := g.table.Match(r.URL.EscapedPath())
	if !ok {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	setRoutePrefix(r.Context(), entry.Route.Prefix)

	// id.Value (the raw key or IP) drives the per-key override lookup;
	// id.String() (fingerprinted) drives the limiter bucket key,
	// namespaced by route so two routes never share a quota. Trusting
	// only RemoteAddr for the IP fallback (identity.go deliberately
	// ignores X-Forwarded-For) assumes turnike runs at the edge: behind
	// a load balancer, every client collapses to the LB's address and
	// would share one bucket.
	id := IdentityFor(r)
	eff := entry.Route.LimitFor(id.Value)
	key := entry.Prefix + ":" + id.String()

	switch dec, err := g.limiter.Allow(r.Context(), key, eff); {
	case err != nil:
		// Fail open: an internal limiter error shouldn't take the
		// upstream down with it. M2's in-memory backend only reaches
		// this for a config-invalid algorithm, which validation already
		// rejects before requests arrive; M3's Redis backend makes
		// fail-open vs fail-closed an explicit policy choice.
		g.logger.ErrorContext(r.Context(), "limiter error", "err", err, "request_id", RequestIDFrom(r.Context()))
	case !dec.Allowed:
		writeRateLimitHeaders(w, dec)
		writeTooManyRequests(w, dec)
		return
	default:
		writeRateLimitHeaders(w, dec)
	}

	g.proxies[entry.Prefix].ServeHTTP(w, r)
}

// writeRateLimitHeaders sets X-RateLimit-* on w. Called before the
// request is proxied so the values survive: ReverseProxy merges the
// upstream's response headers into the same header map with Add, not
// Set, so it never clears what's already there.
func writeRateLimitHeaders(w http.ResponseWriter, dec limiter.Decision) {
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(dec.Limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(dec.Remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(dec.Reset.Unix(), 10))
}

// writeTooManyRequests writes Retry-After and a 429. Callers must set
// X-RateLimit-* via writeRateLimitHeaders first.
func writeTooManyRequests(w http.ResponseWriter, dec limiter.Decision) {
	// Ceil, not round: a client that waits slightly too long is fine, one
	// that retries slightly too early defeats the header's purpose.
	secs := int64(math.Ceil(dec.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// hasDotSegments reports whether the decoded path contains a "." or ".."
// segment.
func hasDotSegments(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// statusClientClosedRequest mirrors nginx's non-standard 499: the client
// disconnected before the gateway produced a response, so no real HTTP
// status was ever put on the wire — but the access log should say that,
// not fall back to the statusRecorder's default 200.
const statusClientClosedRequest = 499

func (g *Gateway) errorHandler(upstream string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		g.logger.ErrorContext(r.Context(), "upstream error",
			"err", err,
			"upstream", upstream,
			"path", r.URL.Path,
			"request_id", RequestIDFrom(r.Context()),
		)
		// The client may be the one that gave up; don't write a 502 into
		// a connection that is already gone. Still correct the access
		// log's idea of what happened: only the recorder's bookkeeping
		// changes here, nothing is written to the (dead) connection.
		if errors.Is(r.Context().Err(), context.Canceled) {
			// The type assertion only succeeds when Gateway is reached
			// through Middleware (handler.go always does this in
			// production); a bare *Gateway in a test just skips the
			// bookkeeping; there's no log line to correct.
			if rec, ok := w.(*statusRecorder); ok {
				rec.status = statusClientClosedRequest
			}
			return
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}
