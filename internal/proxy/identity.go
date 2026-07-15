package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
)

// HeaderAPIKey is the request header carrying the client's API key.
const HeaderAPIKey = "X-API-Key" // #nosec G101 -- header name, not a credential

// Identity is the rate-limit identity of a request: the X-API-Key header
// when present, otherwise the client IP from RemoteAddr.
// X-Forwarded-For is deliberately not trusted — it is client-controlled
// and would let anyone mint fresh rate-limit identities.
type Identity struct {
	Kind  string // "key" or "ip"
	Value string // the raw API key, or the client IP
}

// Identity kinds.
const (
	KindKey = "key"
	KindIP  = "ip"
)

// IdentityFor extracts the identity of r.
func IdentityFor(r *http.Request) Identity {
	if key := r.Header.Get(HeaderAPIKey); key != "" {
		return Identity{Kind: KindKey, Value: key}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr without a port (e.g. in hand-built test requests):
		// use it as-is rather than dropping the identity.
		host = r.RemoteAddr
	}
	return Identity{Kind: KindIP, Value: host}
}

// String renders the identity for logs and (from M3 on) Redis keys:
// "ip:<addr>", or "key:<fingerprint>" with the first 8 bytes of the
// key's SHA-256 — the raw API key never leaves Value, which exists only
// for KeyOverrides lookups.
func (id Identity) String() string {
	if id.Kind == KindKey {
		sum := sha256.Sum256([]byte(id.Value))
		return id.Kind + ":" + hex.EncodeToString(sum[:8])
	}
	return id.Kind + ":" + id.Value
}
