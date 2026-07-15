package proxy

import (
	"testing"

	"github.com/thefcan/turnike/internal/config"
)

func routes(prefixes ...string) []config.Route {
	rs := make([]config.Route, len(prefixes))
	for i, p := range prefixes {
		rs[i] = config.Route{Prefix: p, Upstream: "http://localhost:9000"}
	}
	return rs
}

func TestTableMatch(t *testing.T) {
	tests := []struct {
		name     string
		prefixes []string
		path     string
		want     string // original config prefix of the matched route; "" = miss
	}{
		{"exact match", []string{"/api"}, "/api", "/api"},
		{"trailing slash on request", []string{"/api"}, "/api/", "/api"},
		{"child path", []string{"/api"}, "/api/users", "/api"},
		{"segment boundary blocks sibling", []string{"/api"}, "/apiv2", ""},
		{"segment boundary blocks sibling child", []string{"/api"}, "/apiv2/x", ""},
		{"longest prefix wins", []string{"/api", "/api/v1"}, "/api/v1/x", "/api/v1"},
		{"longest prefix wins regardless of order", []string{"/api/v1", "/api"}, "/api/v1/x", "/api/v1"},
		{"v1beta does not match v1", []string{"/api", "/api/v1"}, "/api/v1beta/x", "/api"},
		{"exact match on longer prefix", []string{"/api", "/api/v1"}, "/api/v1", "/api/v1"},
		{"root matches everything", []string{"/"}, "/anything/at/all", "/"},
		{"root matches itself", []string{"/"}, "/", "/"},
		{"root loses to a longer prefix", []string{"/", "/api"}, "/api/x", "/api"},
		{"unknown path misses", []string{"/api"}, "/unknown", ""},
		{"dot segments cleaned before matching", []string{"/api", "/admin"}, "/api/../admin", "/admin"},
		{"encoded slash is not a boundary", []string{"/api"}, "/api%2Fusers", ""},
		{"trailing slash in config", []string{"/api/"}, "/api", "/api/"},
		{"trailing slash in config with child", []string{"/api/"}, "/api/users", "/api/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table := NewTable(routes(tt.prefixes...))
			entry, ok := table.Match(tt.path)
			if tt.want == "" {
				if ok {
					t.Fatalf("Match(%q) = %q, want miss", tt.path, entry.Route.Prefix)
				}
				return
			}
			if !ok {
				t.Fatalf("Match(%q) missed, want %q", tt.path, tt.want)
			}
			if entry.Route.Prefix != tt.want {
				t.Errorf("Match(%q) = %q, want %q", tt.path, entry.Route.Prefix, tt.want)
			}
		})
	}
}
