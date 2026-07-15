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

// Config is the root of the gateway configuration.
type Config struct {
	Server  Server  `yaml:"server"`
	Limiter Limiter `yaml:"limiter"`
	Routes  []Route `yaml:"routes"`
}

// Server holds HTTP server settings.
type Server struct {
	Listen string `yaml:"listen"`
}

// Limiter selects where rate-limit state lives.
type Limiter struct {
	Backend string `yaml:"backend"`
	Redis   Redis  `yaml:"redis"`
}

// Redis holds connection settings for the redis backend.
type Redis struct {
	Addr string `yaml:"addr"`
}

// Route maps a path prefix to an upstream and its rate-limit policy.
// When multiple prefixes match a request path, the longest one wins.
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
	if c.Limiter.Backend == "" {
		c.Limiter.Backend = BackendMemory
	}
	for i := range c.Routes {
		c.Routes[i].Limit = c.Routes[i].Limit.withDefaults()
	}
}

func (c *Config) validate() error {
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
