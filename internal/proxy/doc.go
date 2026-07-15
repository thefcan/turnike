// Package proxy will hold the reverse proxy core: route table, upstream
// forwarding via httputil.ReverseProxy, and client identity extraction
// (X-API-Key with client-IP fallback). Filled in by milestone M1.
package proxy
