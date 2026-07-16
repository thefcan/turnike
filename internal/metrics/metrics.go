// Package metrics holds the gateway's Prometheus instrumentation: the
// request counter by route and decision, the request-duration
// histogram, the circuit-breaker state gauge and the active-backend
// gauge.
//
// The set is deliberately exactly these four families on a bare
// registry - no Go or process collectors - and every label value is
// drawn from a bounded, config-sized vocabulary. Client identity (API
// key or IP) is never a label: identity is unauthenticated client
// input, so labeling by it would let any caller mint time series the
// same way an unbounded state map would mint gateway memory - the
// maxKeys threat model, at the TSDB layer.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Decision label values for RequestsTotal. allow and deny are the
// configured backend's own quota verdict. The degrade_* pair is the
// degrade fallback's verdict - real allow/deny decisions, kept apart
// so a redis outage doesn't erase the 429 rate from the graphs. Bare
// degrade means no verdict existed at all: the fail_open pass-through
// (including the memory backend's at-capacity guard) and the
// fail_closed 503. The degrade family is therefore wider than the
// on_error policy of the same name - whichever failure policy is
// configured produces these values when the primary backend cannot
// answer.
const (
	DecisionAllow        = "allow"
	DecisionDeny         = "deny"
	DecisionDegradeAllow = "degrade_allow"
	DecisionDegradeDeny  = "degrade_deny"
	DecisionDegrade      = "degrade"
)

// Backend label values for LimiterBackend; they mirror the
// config.Backend* config-file values.
const (
	BackendRedis  = "redis"
	BackendMemory = "memory"
)

var (
	// decisions lists every RequestsTotal decision value, for
	// pre-materializing series: rate() needs samples at both ends of
	// its range, so a series that first appears on its first
	// increment hides that increment from the graph.
	decisions = []string{DecisionAllow, DecisionDeny, DecisionDegradeAllow, DecisionDegradeDeny, DecisionDegrade}
	// backends lists every LimiterBackend backend value.
	backends = []string{BackendRedis, BackendMemory}
)

// Metrics is the gateway's instrument set on its own registry. One
// instance is built in cmd/gateway and constructor-threaded to the
// proxy and limiter packages - no globals, so tests build their own
// and never fight over a shared default registry.
type Metrics struct {
	Registry *prometheus.Registry

	// RequestsTotal counts requests that reached the rate-limit
	// decision point, by configured route prefix and decision.
	// Unrouted (404) and reserved paths are not counted.
	RequestsTotal *prometheus.CounterVec
	// RequestDuration observes the gateway answer duration for
	// exactly the requests RequestsTotal counts - all outcomes, so
	// fast denials pull the quantiles down as real answers.
	RequestDuration prometheus.Histogram
	// BreakerState mirrors the redis circuit breaker's own state
	// constants: 0 closed, 1 open, 2 half-open. Constant 0 under the
	// memory backend, where no breaker exists.
	BreakerState prometheus.Gauge
	// LimiterBackend is a one-hot pair: 1 for the backend that
	// answered the most recent rate-limit decision. Last decision
	// wins - concurrent flips may interleave, so the pair is
	// eventually one-hot, not atomically so.
	LimiterBackend *prometheus.GaugeVec
}

// New builds the instrument set on a fresh registry holding exactly
// these four families. Both LimiterBackend series are materialized at
// 0 here; the limiter marks the configured backend active when it is
// built.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "turnike",
			Name:      "requests_total",
			Help: "Requests that reached the rate-limit decision point, by configured route " +
				"prefix and decision. allow/deny are the configured backend's own verdict; " +
				"degrade_allow/degrade_deny are the degrade fallback's verdict; degrade means " +
				"no verdict existed (fail_open pass-through or fail_closed 503). Unrouted and " +
				"reserved paths are not counted.",
		}, []string{"route", "decision"}),
		RequestDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "turnike",
			Name:      "request_duration_seconds",
			Help: "Gateway answer duration in seconds for routed requests, all outcomes: " +
				"allowed answers include upstream time, denials return without proxying.",
			// DefBuckets starts at 5ms; loopback answers should sit
			// well under that and would pile into one bucket, so
			// finer low-end buckets are prepended. The 10s tail
			// stays: response_header_timeout makes anything slower
			// uniformly "broken".
			Buckets: append([]float64{0.0005, 0.001, 0.0025}, prometheus.DefBuckets...),
		}),
		BreakerState: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "turnike",
			Name:      "breaker_state",
			Help: "Redis circuit breaker state: 0=closed, 1=open, 2=half-open. Constant 0 " +
				"under the memory backend, where no breaker exists.",
		}),
		LimiterBackend: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "turnike",
			Name:      "limiter_backend",
			Help: "1 for the backend that answered the most recent rate-limit decision " +
				"(redis flips to memory while the degrade fallback answers). Last decision " +
				"wins: the pair is eventually one-hot, not atomically so.",
		}, []string{"backend"}),
	}
	m.Registry.MustRegister(m.RequestsTotal, m.RequestDuration, m.BreakerState, m.LimiterBackend)
	for _, b := range backends {
		m.LimiterBackend.WithLabelValues(b)
	}
	return m
}

// Handler serves the registry in the Prometheus text exposition
// format. promhttp stays confined to this package - and note it
// answers any HTTP method, so the caller owns the GET/HEAD gate.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// SetActiveBackend records which backend answered the most recent
// decision. Both series go through here so a flip is one code path;
// concurrent callers may interleave the two writes, which is why the
// one-hot shape is "last decision wins", never a hard invariant.
func (m *Metrics) SetActiveBackend(active string) {
	for _, b := range backends {
		var v float64
		if b == active {
			v = 1
		}
		m.LimiterBackend.WithLabelValues(b).Set(v)
	}
}

// InitRoute pre-materializes every decision series for a configured
// route at 0, so rate() sees each series from the first scrape
// instead of discovering it on its first increment.
func (m *Metrics) InitRoute(route string) {
	for _, d := range decisions {
		m.RequestsTotal.WithLabelValues(route, d)
	}
}
