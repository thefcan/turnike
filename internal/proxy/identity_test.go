package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
)

// keyFingerprint mirrors Identity.String's hashing so tests can pin the
// exact log form of an API key.
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "key:" + hex.EncodeToString(sum[:8])
}

func TestIdentityFor(t *testing.T) {
	tests := []struct {
		name       string
		apiKey     string
		remoteAddr string
		want       Identity
		wantString string
	}{
		{
			name:       "api key wins",
			apiKey:     "demo-premium-key",
			remoteAddr: "1.2.3.4:5678",
			want:       Identity{Kind: KindKey, Value: "demo-premium-key"},
			wantString: keyFingerprint("demo-premium-key"),
		},
		{
			name:       "ip fallback strips port",
			remoteAddr: "1.2.3.4:5678",
			want:       Identity{Kind: KindIP, Value: "1.2.3.4"},
			wantString: "ip:1.2.3.4",
		},
		{
			name:       "ipv6 fallback strips brackets and port",
			remoteAddr: "[::1]:80",
			want:       Identity{Kind: KindIP, Value: "::1"},
			wantString: "ip:::1",
		},
		{
			name:       "malformed remote addr used as-is",
			remoteAddr: "not-an-addr",
			want:       Identity{Kind: KindIP, Value: "not-an-addr"},
			wantString: "ip:not-an-addr",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.apiKey != "" {
				r.Header.Set(HeaderAPIKey, tt.apiKey)
			}
			got := IdentityFor(r)
			if got != tt.want {
				t.Errorf("IdentityFor = %+v, want %+v", got, tt.want)
			}
			if got.String() != tt.wantString {
				t.Errorf("String() = %q, want %q", got.String(), tt.wantString)
			}
		})
	}
}

func TestIdentityStringNeverLeaksRawKey(t *testing.T) {
	id := Identity{Kind: KindKey, Value: "super-secret-api-key"}
	if s := id.String(); strings.Contains(s, "super-secret") {
		t.Errorf("String() = %q leaks the raw API key", s)
	}
}

func TestIdentityForIgnoresXForwardedFor(t *testing.T) {
	r := httptest.NewRequest("GET", "/api", nil)
	r.RemoteAddr = "1.2.3.4:5678"
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := IdentityFor(r); got.Value != "1.2.3.4" {
		t.Errorf("IdentityFor trusted X-Forwarded-For: got %+v", got)
	}
}
