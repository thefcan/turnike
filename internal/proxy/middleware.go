package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/thefcan/turnike/internal/metrics"
)

// HeaderRequestID is the header carrying the request ID; an inbound value
// matching requestIDPattern is honored, anything else is replaced with a
// fresh ID.
const HeaderRequestID = "X-Request-Id"

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	routePrefixKey
)

// Middleware wraps next with request-ID handling, a per-request access
// log line and the request-duration observation. The ID is set on the
// response and on the request headers (so the reverse proxy forwards it
// upstream) and is available downstream via RequestIDFrom.
func Middleware(logger *slog.Logger, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := r.Header.Get(HeaderRequestID)
			if !requestIDPattern.MatchString(id) {
				id = newRequestID()
			}
			r.Header.Set(HeaderRequestID, id)
			w.Header().Set(HeaderRequestID, id)

			// The matched route is only known further down the chain; the
			// gateway writes it through this holder so it lands in the log.
			routePrefix := new(string)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			ctx = context.WithValue(ctx, routePrefixKey, routePrefix)
			r = r.WithContext(ctx)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			// Deferred so the line survives ReverseProxy's ErrAbortHandler
			// panic on a failed body copy (the panic still propagates).
			defer func() {
				elapsed := time.Since(start)
				// Observed only for requests that matched a route, so
				// histogram observations mirror requests_total increments
				// exactly: 404s and dot-segment rejects observe nothing,
				// and reserved paths never enter this middleware at all.
				if *routePrefix != "" {
					m.RequestDuration.Observe(elapsed.Seconds())
				}
				logger.LogAttrs(ctx, slog.LevelInfo, "request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", rec.status),
					slog.Duration("duration", elapsed),
					slog.String("route", *routePrefix),
					slog.String("identity", IdentityFor(r).String()),
					slog.String("request_id", id),
					slog.Int64("bytes", rec.bytes),
				)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// RequestIDFrom returns the request ID stored by Middleware, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// setRoutePrefix records the matched route's configured prefix for the
// access log. A no-op when the request did not pass through Middleware.
func setRoutePrefix(ctx context.Context, prefix string) {
	if p, ok := ctx.Value(routePrefixKey).(*string); ok {
		*p = prefix
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}

// statusRecorder captures the status code and body size for the access
// log while delegating everything else to the wrapped writer.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer's
// Flusher, which ReverseProxy needs for streaming responses.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
