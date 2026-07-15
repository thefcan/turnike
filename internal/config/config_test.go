package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
server:
  listen: ":8080"
limiter:
  backend: memory
routes:
  - prefix: /api/
    upstream: http://localhost:9000
    limit:
      algorithm: token_bucket
      rate: 10
      burst: 20
    key_overrides:
      premium:
        rate: 100
        burst: 200
  - prefix: /search/
    upstream: http://localhost:9000
    limit:
      algorithm: sliding_window
      rate: 5
      window: 10s
`

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring the error must contain; empty = expect success
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name: "valid config",
			yaml: validYAML,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if len(cfg.Routes) != 2 {
					t.Fatalf("routes = %d, want 2", len(cfg.Routes))
				}
				if got := cfg.Routes[0].Limit.Algorithm; got != AlgoTokenBucket {
					t.Errorf("routes[0] algorithm = %q, want %q", got, AlgoTokenBucket)
				}
				if got := time.Duration(cfg.Routes[0].Limit.Window); got != time.Second {
					t.Errorf("routes[0] window = %v, want the 1s token_bucket default", got)
				}
				if got := time.Duration(cfg.Routes[1].Limit.Window); got != 10*time.Second {
					t.Errorf("routes[1] window = %v, want 10s", got)
				}
			},
		},
		{
			name: "defaults applied",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Server.Listen != ":8080" {
					t.Errorf("listen = %q, want :8080", cfg.Server.Listen)
				}
				if cfg.Limiter.Backend != BackendMemory {
					t.Errorf("backend = %q, want %q", cfg.Limiter.Backend, BackendMemory)
				}
			},
		},
		{
			name:    "unknown field rejected",
			yaml:    "routez: []\n",
			wantErr: "not found in type",
		},
		{
			name:    "empty document",
			yaml:    "",
			wantErr: "empty document",
		},
		{
			name:    "no routes",
			yaml:    "server: {listen: \":8080\"}\n",
			wantErr: "at least one route",
		},
		{
			name: "prefix must start with slash",
			yaml: `
routes:
  - prefix: api/
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`,
			wantErr: "must start with",
		},
		{
			name: "upstream must be absolute URL",
			yaml: `
routes:
  - prefix: /
    upstream: localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`,
			wantErr: "absolute http(s) URL",
		},
		{
			name: "unknown algorithm",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: leaky_bucket, rate: 1, burst: 1}
`,
			wantErr: "unknown algorithm",
		},
		{
			name: "rate must be positive",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 0, burst: 1}
`,
			wantErr: "rate must be > 0",
		},
		{
			name: "token bucket requires burst",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1}
`,
			wantErr: "burst > 0",
		},
		{
			name: "sliding window requires window",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: sliding_window, rate: 1}
`,
			wantErr: "window > 0",
		},
		{
			name: "burst rejected on window algorithms",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: sliding_window, rate: 5, window: 10s, burst: 3}
`,
			wantErr: "burst is only valid for token_bucket",
		},
		{
			name: "token bucket accepts an explicit refill window",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 100, burst: 20, window: 1m}
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if got := time.Duration(cfg.Routes[0].Limit.Window); got != time.Minute {
					t.Errorf("window = %v, want 1m", got)
				}
			},
		},
		{
			name:    "multiple yaml documents rejected",
			yaml:    validYAML + "\n---\nserver: {listen: \":9\"}\n",
			wantErr: "multiple YAML documents",
		},
		{
			name: "trailing-slash prefix collision rejected",
			yaml: `
routes:
  - prefix: /api
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
  - prefix: /api/
    upstream: http://localhost:9001
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`,
			wantErr: "duplicate prefix",
		},
		{
			name: "algorithm-switching override must be self-contained",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
    key_overrides:
      strict: {algorithm: fixed_window, rate: 5}
`,
			wantErr: "key_overrides[strict]: fixed_window requires window",
		},
		{
			name: "invalid duration string",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: fixed_window, rate: 1, window: 5x}
`,
			wantErr: "invalid duration",
		},
		{
			name: "redis backend requires addr",
			yaml: `
limiter:
  backend: redis
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`,
			wantErr: "requires redis.addr",
		},
		{
			name: "redis backend with addr is valid",
			yaml: `
limiter:
  backend: redis
  redis: {addr: "localhost:6379"}
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Limiter.Redis.Addr != "localhost:6379" {
					t.Errorf("redis addr = %q", cfg.Limiter.Redis.Addr)
				}
			},
		},
		{
			name: "unknown backend",
			yaml: `
limiter:
  backend: memcached
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`,
			wantErr: "unknown backend",
		},
		{
			name: "duplicate prefix",
			yaml: `
routes:
  - prefix: /api/
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
  - prefix: /api/
    upstream: http://localhost:9001
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
`,
			wantErr: "duplicate prefix",
		},
		{
			name: "invalid merged override rejected",
			yaml: `
routes:
  - prefix: /
    upstream: http://localhost:9000
    limit: {algorithm: token_bucket, rate: 1, burst: 1}
    key_overrides:
      bad: {rate: -5}
`,
			wantErr: "key_overrides[bad]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestLimitFor(t *testing.T) {
	base := Limit{Algorithm: AlgoTokenBucket, Rate: 10, Burst: 20}
	route := Route{
		Prefix:   "/api/",
		Upstream: "http://localhost:9000",
		Limit:    base,
		KeyOverrides: map[string]Limit{
			"partial": {Rate: 100},
			"full":    {Algorithm: AlgoFixedWindow, Rate: 50, Window: Duration(time.Minute)},
		},
	}

	second := Duration(time.Second)
	tests := []struct {
		name string
		key  string
		want Limit
	}{
		{
			"unknown key gets base limit with defaults",
			"nobody",
			Limit{Algorithm: AlgoTokenBucket, Rate: 10, Burst: 20, Window: second},
		},
		{
			"partial override inherits the rest",
			"partial",
			Limit{Algorithm: AlgoTokenBucket, Rate: 100, Burst: 20, Window: second},
		},
		{
			// Switching algorithm must not drag token_bucket's burst into
			// a window limit — the override is taken as-is.
			"algorithm switch does not inherit base fields",
			"full",
			Limit{Algorithm: AlgoFixedWindow, Rate: 50, Window: Duration(time.Minute)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := route.LimitFor(tt.key); got != tt.want {
				t.Errorf("LimitFor(%q) = %+v, want %+v", tt.key, got, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Run("reads and validates a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(validYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Routes) != 2 {
			t.Errorf("routes = %d, want 2", len(cfg.Routes))
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Fatal("want error for missing file, got nil")
		}
	})
}
