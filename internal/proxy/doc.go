// Package proxy holds the reverse proxy core: the segment-boundary
// longest-prefix route table, upstream forwarding via
// httputil.ReverseProxy, and client identity extraction (X-API-Key with
// client-IP fallback).
package proxy
