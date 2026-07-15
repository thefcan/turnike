package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thefcan/turnike/internal/config"
)

// echoUpstream mirrors mock/main.go: it reports what it received so
// routing and header passthrough are observable from the test.
type echoPayload struct {
	Marker  string      `json:"marker"`
	Path    string      `json:"path"`
	Query   string      `json:"query"`
	Host    string      `json:"host"`
	Headers http.Header `json:"headers"`
}

func echoUpstream(t *testing.T, marker string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(echoPayload{
			Marker:  marker,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Host:    r.Host,
			Headers: r.Header,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestGateway(t *testing.T, routes []config.Route) *Gateway {
	t.Helper()
	g, err := NewGateway(routes, config.Upstream{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return g
}

func decodeEcho(t *testing.T, resp *http.Response) echoPayload {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var p echoPayload
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	return p
}

func TestGatewayRouting(t *testing.T) {
	api := echoUpstream(t, "api")
	search := echoUpstream(t, "search")
	g := newTestGateway(t, []config.Route{
		{Prefix: "/api/", Upstream: api.URL},
		{Prefix: "/search/", Upstream: search.URL},
	})
	front := httptest.NewServer(g)
	t.Cleanup(front.Close)

	tests := []struct {
		path       string
		wantMarker string
	}{
		{"/api/users?q=1", "api"},
		{"/search/x", "search"},
	}
	for _, tt := range tests {
		resp, err := http.Get(front.URL + tt.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tt.path, err)
		}
		p := decodeEcho(t, resp)
		if p.Marker != tt.wantMarker {
			t.Errorf("GET %s hit %q, want %q", tt.path, p.Marker, tt.wantMarker)
		}
		wantPath, wantQuery, _ := strings.Cut(tt.path, "?")
		if p.Path != wantPath || p.Query != wantQuery {
			t.Errorf("upstream saw path %q query %q, want %q %q", p.Path, p.Query, wantPath, wantQuery)
		}
	}
}

func TestGatewayHeaderPassthroughAndForwarded(t *testing.T) {
	up := echoUpstream(t, "api")
	g := newTestGateway(t, []config.Route{{Prefix: "/api/", Upstream: up.URL}})
	front := httptest.NewServer(g)
	t.Cleanup(front.Close)

	req, _ := http.NewRequest("GET", front.URL+"/api/x", nil)
	req.Header.Set("X-Custom", "hello")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	p := decodeEcho(t, resp)

	if got := p.Headers.Get("X-Custom"); got != "hello" {
		t.Errorf("X-Custom = %q, want passthrough of %q", got, "hello")
	}
	if got := p.Headers.Get("X-Forwarded-For"); got == "" {
		t.Error("X-Forwarded-For missing at upstream")
	}
	frontHost := strings.TrimPrefix(front.URL, "http://")
	if got := p.Headers.Get("X-Forwarded-Host"); got != frontHost {
		t.Errorf("X-Forwarded-Host = %q, want original host %q", got, frontHost)
	}
	if got := p.Headers.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http", got)
	}
	upstreamHost := strings.TrimPrefix(up.URL, "http://")
	if p.Host != upstreamHost {
		t.Errorf("upstream Host = %q, want rewritten to %q", p.Host, upstreamHost)
	}
}

func TestGatewayUnknownRoute(t *testing.T) {
	up := echoUpstream(t, "api")
	g := newTestGateway(t, []config.Route{{Prefix: "/api/", Upstream: up.URL}})

	for _, path := range []string{"/nope", "/apiv2"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
	}
}

func TestGatewayDeadUpstream(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	_ = l.Close()

	g := newTestGateway(t, []config.Route{{Prefix: "/api/", Upstream: dead}})
	r := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("dead upstream = %d, want 502", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "bad gateway") {
		t.Errorf("body = %q, want bad gateway message", body)
	}
}
