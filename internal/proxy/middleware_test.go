package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestMiddlewareRequestID(t *testing.T) {
	tests := []struct {
		name      string
		inboundID string
		wantFresh bool
	}{
		{"generates an id when absent", "", true},
		{"honors a reasonable inbound id", "client-supplied-id", false},
		{"replaces an oversized inbound id", strings.Repeat("x", 129), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seenByHandler, seenInCtx string
			h := Middleware(slog.New(slog.DiscardHandler))(
				http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					seenByHandler = r.Header.Get(HeaderRequestID)
					seenInCtx = RequestIDFrom(r.Context())
				}))

			r := httptest.NewRequest("GET", "/api", nil)
			if tt.inboundID != "" {
				r.Header.Set(HeaderRequestID, tt.inboundID)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			got := w.Header().Get(HeaderRequestID)
			if tt.wantFresh {
				if !hexID.MatchString(got) {
					t.Errorf("response id = %q, want generated 32-hex", got)
				}
			} else if got != tt.inboundID {
				t.Errorf("response id = %q, want inbound %q", got, tt.inboundID)
			}
			if seenByHandler != got {
				t.Errorf("request header id = %q, response id = %q; want equal", seenByHandler, got)
			}
			if seenInCtx != got {
				t.Errorf("context id = %q, response id = %q; want equal", seenInCtx, got)
			}
		})
	}
}

func TestMiddlewareAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setRoutePrefix(r.Context(), "/api/")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))

	r := httptest.NewRequest("GET", "/api/users?q=1", nil)
	r.RemoteAddr = "1.2.3.4:5678"
	r.Header.Set(HeaderAPIKey, "demo")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var line struct {
		Msg       string `json:"msg"`
		Method    string `json:"method"`
		Path      string `json:"path"`
		Status    int    `json:"status"`
		Route     string `json:"route"`
		Identity  string `json:"identity"`
		RequestID string `json:"request_id"`
		Bytes     int64  `json:"bytes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("access log is not one JSON line: %v\n%s", err, buf.String())
	}
	if line.Msg != "request" || line.Method != "GET" || line.Path != "/api/users" {
		t.Errorf("unexpected msg/method/path: %+v", line)
	}
	if line.Status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", line.Status, http.StatusTeapot)
	}
	if line.Route != "/api/" {
		t.Errorf("route = %q, want /api/", line.Route)
	}
	if line.Identity != "key:demo" {
		t.Errorf("identity = %q, want key:demo", line.Identity)
	}
	if line.RequestID == "" {
		t.Error("request_id missing from access log")
	}
	if line.Bytes != int64(len("hello")) {
		t.Errorf("bytes = %d, want %d", line.Bytes, len("hello"))
	}
}

func TestStatusRecorderDefaultsAndUnwrap(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if rec.status != http.StatusOK {
		t.Errorf("default status = %d, want 200", rec.status)
	}
	rc := http.NewResponseController(rec)
	if err := rc.Flush(); err != nil {
		t.Errorf("Flush through Unwrap failed: %v", err)
	}
}
