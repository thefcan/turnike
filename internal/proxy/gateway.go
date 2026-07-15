package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/thefcan/turnike/internal/config"
)

// Gateway routes requests to their upstream via one reverse proxy per
// route. Unmatched paths get a 404, upstream failures a 502.
type Gateway struct {
	table   *Table
	proxies map[string]*httputil.ReverseProxy // keyed by normalized Entry.Prefix
	logger  *slog.Logger
}

// NewGateway compiles routes into a gateway. All routes share one
// transport (and thus one connection pool) with the given timeouts.
func NewGateway(routes []config.Route, up config.Upstream, logger *slog.Logger) (*Gateway, error) {
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
	g.proxies[entry.Prefix].ServeHTTP(w, r)
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

func (g *Gateway) errorHandler(upstream string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		g.logger.ErrorContext(r.Context(), "upstream error",
			"err", err,
			"upstream", upstream,
			"path", r.URL.Path,
			"request_id", RequestIDFrom(r.Context()),
		)
		// The client may be the one that gave up; don't write a 502 into
		// a connection that is already gone.
		if errors.Is(r.Context().Err(), context.Canceled) {
			return
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}
