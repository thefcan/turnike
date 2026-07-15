package proxy

import (
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

// String renders the identity as "key:<apikey>" or "ip:<addr>" — the
// collision-proof form used in logs and (from M3 on) Redis keys.
func (id Identity) String() string {
	return id.Kind + ":" + id.Value
}
