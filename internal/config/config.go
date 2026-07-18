// Package config loads and validates the gateway's YAML configuration:
// listen address, limiter backend selection, and the route table with
// per-route limits and per-API-key overrides.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Supported rate-limiting algorithms.
const (
	AlgoFixedWindow   = "fixed_window"
	AlgoSlidingWindow = "sliding_window"
	AlgoTokenBucket   = "token_bucket"
)

// Supported limiter state backends.
const (
	BackendMemory = "memory"
	BackendRedis  = "redis"
)

// Supported redis failure policies: what the gateway does with a request
// when the redis backend cannot answer (call failed or circuit open).
const (
	OnErrorFailOpen   = "fail_open"   // proxy the request without limiting
	OnErrorFailClosed = "fail_closed" // reject the request with 503
	OnErrorDegrade    = "degrade"     // fall back to per-instance in-memory limiting
)

// Config is the root of the gateway configuration.
type Config struct {
	Server   Server   `yaml:"server"`
	Upstream Upstream `yaml:"upstream"`
	Limiter  Limiter  `yaml:"limiter"`
	Routes   []Route  `yaml:"routes"`
}

// Server holds HTTP server settings. Zero timeouts are replaced with the
// defaults noted below; timeouts cannot be disabled in M1.
type Server struct {
	Listen string `yaml:"listen"`
	// MetricsDisabled gates the /metrics endpoint off the client-facing
	// listener. There is no separate admin listener, so /metrics otherwise
	// shares the data-plane port; the public deployment sets this true so a
	// Prometheus scrape endpoint is never reachable from the internet (Fly
	// does not path-block, and nothing scrapes prod - Grafana stays local).
	// The zero value keeps /metrics served, which every non-prod config
	// expects.
	MetricsDisabled   bool     `yaml:"metrics_disabled"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"` // default 5s
	ReadTimeout       Duration `yaml:"read_timeout"`        // default 30s
	WriteTimeout      Duration `yaml:"write_timeout"`       // default 60s; also caps proxied response time
	IdleTimeout       Duration `yaml:"idle_timeout"`        // default 120s
	ShutdownTimeout   Duration `yaml:"shutdown_timeout"`    // default 10s
}

// Upstream holds HTTP client settings for talking to upstreams; one
// transport with these timeouts is shared across all routes.
type Upstream struct {
	DialTimeout           Duration `yaml:"dial_timeout"`            // default 5s
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout"` // default 10s
}

// Limiter selects where rate-limit state lives.
type Limiter struct {
	Backend string `yaml:"backend"`
	Redis   Redis  `yaml:"redis"`
}

// Redis holds connection settings for the redis backend.
type Redis struct {
	Addr string `yaml:"addr"`
	// OnError is the failure policy applied when redis cannot answer:
	// one of fail_open, fail_closed or degrade (the default). See the
	// OnError* constants.
	OnError string `yaml:"on_error"`
}

// Route maps a path prefix to an upstream and its rate-limit policy.
//
// Matching is segment-boundary longest-prefix: a request path matches a
// prefix when it equals the prefix or continues it after a "/" — so
// "/api" matches "/api" and "/api/users" but not "/apiv2", and "/api/v1"
// does not match "/api/v1beta/x". Trailing slashes are insignificant
// ("/api" == "/api/"), and when several prefixes match, the longest wins.
// A prefix of "/" matches every path. Matching runs on the cleaned,
// escaped request path; the URL is forwarded to the upstream unchanged
// (no prefix stripping). If the upstream URL itself has a path, the
// request path is appended to it.
type Route struct {
	Prefix       string           `yaml:"prefix"`
	Upstream     string           `yaml:"upstream"`
	Limit        Limit            `yaml:"limit"`
	KeyOverrides map[string]Limit `yaml:"key_overrides"`
}

// Limit describes one rate-limit policy. Rate is the number of requests
// allowed per Window. For token_bucket, Window is the refill interval
// (defaults to 1s) and Burst is the bucket capacity; Burst is invalid for
// the window algorithms.
type Limit struct {
	Algorithm string   `yaml:"algorithm"`
	Rate      int      `yaml:"rate"`
	Burst     int      `yaml:"burst"`
	Window    Duration `yaml:"window"`
}

// Duration is a time.Duration that unmarshals from YAML strings like
// "500ms" or "1m".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// Load reads, parses and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the operator-supplied -config flag
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse parses and validates raw YAML. Unknown fields are rejected so a
// typo fails fast instead of silently disabling a limit.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("parse config: empty document")
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// A second document would be dropped silently otherwise — reject it.
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse config: multiple YAML documents (want exactly one)")
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

// LimitFor returns the effective limit for the given client key: the
// per-key override merged over the route's base limit. Zero-valued
// override fields inherit from the base. An override that switches to a
// different algorithm is taken as-is (plus defaults) — inheriting numbers
// across algorithms would silently mix semantics.
func (r Route) LimitFor(key string) Limit {
	o, ok := r.KeyOverrides[key]
	if !ok {
		return r.Limit.withDefaults()
	}
	if o.Algorithm != "" && o.Algorithm != r.Limit.Algorithm {
		return o.withDefaults()
	}
	merged := r.Limit
	if o.Rate != 0 {
		merged.Rate = o.Rate
	}
	if o.Burst != 0 {
		merged.Burst = o.Burst
	}
	if o.Window != 0 {
		merged.Window = o.Window
	}
	return merged.withDefaults()
}

// withDefaults fills algorithm-specific defaults: token_bucket refills
// once per second unless an explicit window is given.
func (l Limit) withDefaults() Limit {
	if l.Algorithm == AlgoTokenBucket && l.Window == 0 {
		l.Window = Duration(time.Second)
	}
	return l
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	defaultDuration(&c.Server.ReadHeaderTimeout, 5*time.Second)
	defaultDuration(&c.Server.ReadTimeout, 30*time.Second)
	defaultDuration(&c.Server.WriteTimeout, 60*time.Second)
	defaultDuration(&c.Server.IdleTimeout, 120*time.Second)
	defaultDuration(&c.Server.ShutdownTimeout, 10*time.Second)
	defaultDuration(&c.Upstream.DialTimeout, 5*time.Second)
	defaultDuration(&c.Upstream.ResponseHeaderTimeout, 10*time.Second)
	if c.Limiter.Backend == "" {
		c.Limiter.Backend = BackendMemory
	}
	if c.Limiter.Redis.OnError == "" {
		c.Limiter.Redis.OnError = OnErrorDegrade
	}
	for i := range c.Routes {
		c.Routes[i].Limit = c.Routes[i].Limit.withDefaults()
	}
}

func defaultDuration(d *Duration, def time.Duration) {
	if *d == 0 {
		*d = Duration(def)
	}
}

func (c *Config) validate() error {
	for _, tt := range []struct {
		name string
		d    Duration
	}{
		{"server.read_header_timeout", c.Server.ReadHeaderTimeout},
		{"server.read_timeout", c.Server.ReadTimeout},
		{"server.write_timeout", c.Server.WriteTimeout},
		{"server.idle_timeout", c.Server.IdleTimeout},
		{"server.shutdown_timeout", c.Server.ShutdownTimeout},
		{"upstream.dial_timeout", c.Upstream.DialTimeout},
		{"upstream.response_header_timeout", c.Upstream.ResponseHeaderTimeout},
	} {
		if tt.d < 0 {
			return fmt.Errorf("%s must not be negative, got %v", tt.name, time.Duration(tt.d))
		}
	}
	switch c.Limiter.Backend {
	case BackendMemory:
	case BackendRedis:
		if c.Limiter.Redis.Addr == "" {
			return errors.New("limiter: redis backend requires redis.addr")
		}
	default:
		return fmt.Errorf("limiter: unknown backend %q (want %q or %q)",
			c.Limiter.Backend, BackendMemory, BackendRedis)
	}
	// Validated regardless of backend so a typo dies at parse time, not
	// on the day the backend is switched to redis.
	switch c.Limiter.Redis.OnError {
	case OnErrorFailOpen, OnErrorFailClosed, OnErrorDegrade:
	default:
		return fmt.Errorf("limiter: redis.on_error must be %q, %q or %q, got %q",
			OnErrorFailOpen, OnErrorFailClosed, OnErrorDegrade, c.Limiter.Redis.OnError)
	}
	if len(c.Routes) == 0 {
		return errors.New("routes: at least one route is required")
	}
	seen := make(map[string]struct{}, len(c.Routes))
	for i, r := range c.Routes {
		if err := r.validate(); err != nil {
			return fmt.Errorf("routes[%d]: %w", i, err)
		}
		// "/api" and "/api/" would shadow each other in prefix matching,
		// so a trailing slash is ignored when checking uniqueness.
		norm := strings.TrimRight(r.Prefix, "/")
		if _, dup := seen[norm]; dup {
			return fmt.Errorf("routes[%d]: duplicate prefix %q (trailing slash is ignored when comparing)", i, r.Prefix)
		}
		seen[norm] = struct{}{}
	}
	return nil
}

func (r Route) validate() error {
	if !strings.HasPrefix(r.Prefix, "/") {
		return fmt.Errorf("prefix %q must start with %q", r.Prefix, "/")
	}
	u, err := url.Parse(r.Upstream)
	if err != nil {
		return fmt.Errorf("upstream %q: %w", r.Upstream, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("upstream %q must be an absolute http(s) URL", r.Upstream)
	}
	if err := r.Limit.validate(); err != nil {
		return fmt.Errorf("limit: %w", err)
	}
	for key := range r.KeyOverrides {
		if key == "" {
			return errors.New("key_overrides: empty API key")
		}
		// Overrides are validated post-merge: a partial override is legal
		// as long as the effective limit it produces is.
		if err := r.LimitFor(key).validate(); err != nil {
			return fmt.Errorf("key_overrides[%s]: %w", key, err)
		}
	}
	return nil
}

func (l Limit) validate() error {
	if l.Rate <= 0 {
		return fmt.Errorf("rate must be > 0, got %d", l.Rate)
	}
	switch l.Algorithm {
	case AlgoTokenBucket:
		if l.Burst <= 0 {
			return fmt.Errorf("token_bucket requires burst > 0, got %d", l.Burst)
		}
		if l.Window <= 0 {
			return errors.New("token_bucket requires window > 0 (the refill interval, default 1s)")
		}
	case AlgoFixedWindow, AlgoSlidingWindow:
		if l.Window <= 0 {
			return fmt.Errorf("%s requires window > 0", l.Algorithm)
		}
		if l.Burst != 0 {
			return fmt.Errorf("burst is only valid for token_bucket, got burst %d on %s", l.Burst, l.Algorithm)
		}
	default:
		return fmt.Errorf("unknown algorithm %q (want %q, %q or %q)",
			l.Algorithm, AlgoTokenBucket, AlgoFixedWindow, AlgoSlidingWindow)
	}
	return nil
}
