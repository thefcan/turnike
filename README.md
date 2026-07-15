# turnike

**turnike** — a turnstile for your APIs. A distributed rate limiter & API
gateway in Go, built in public. On the roadmap: hand-implemented token bucket,
sliding window and fixed window algorithms, atomic shared state via Redis+Lua,
and burst-load benchmarks against a multi-replica setup.

> 🚧 Work in progress. The full design-doc README (problem statement,
> architecture, measured trade-offs, multi-instance bypass demo) lands with
> milestone M6 — see [PROGRESS.md](PROGRESS.md) for current status.

## Routing

Routes map path prefixes to upstreams (see
[config.example.yaml](config.example.yaml)). Matching is
**segment-boundary longest-prefix**: a prefix matches whole path segments
only, so `/api` matches `/api` and `/api/users` but not `/apiv2`, and
`/api/v1` never captures `/api/v1beta/x`. Trailing slashes are
insignificant, and when several prefixes match, the longest wins. The
request URL is forwarded to the upstream unchanged (no prefix stripping).

`/healthz` and `/readyz` are reserved gateway endpoints and take
precedence over configured routes. Client identity is the `X-API-Key`
header, falling back to the client IP (`X-Forwarded-For` is not trusted).
Every proxied request gets an `X-Request-Id` (a well-formed inbound value
is honored) and one structured access-log line; the raw API key never
appears in logs, only a fingerprint.

## Rate limiting

Every route carries a [`Limit`](config.example.yaml): **fixed_window**,
**sliding_window** (a timestamp log — exact, never over-admits across a
window boundary the way fixed_window's fixed grid can) or **token_bucket**
(a continuous refill up to a burst capacity). `key_overrides` swaps in a
different limit for specific API keys. All three run in memory today,
behind a `Limiter` interface designed to move state into Redis (M3)
without changing the request path.

Every response for a matched route carries `X-RateLimit-Limit` /
`X-RateLimit-Remaining` / `X-RateLimit-Reset` (rate for the window
algorithms, burst for token_bucket); a denied request also gets
`Retry-After` and a `429`. The gateway sets these before proxying and
owns the names — an upstream that sets its own `X-RateLimit-*` gets a
second value appended alongside the gateway's, not a replacement.

Rate limiting assumes **turnike runs at the edge**: client identity is the
`X-API-Key` header, falling back to `RemoteAddr` — `X-Forwarded-For` is
deliberately not trusted, since it's client-controlled and would let
anyone mint fresh identities. Placing turnike behind another load
balancer collapses every client to that balancer's address, and they'd
share one quota; a `trusted_proxies` config knob to opt back into
`X-Forwarded-For` is a natural follow-up if that setup is ever needed.

The in-memory backend keeps one bucket per (route, identity), capped at
100,000 distinct entries — identity isn't authenticated, so nothing but
this cap stops a caller from growing it by varying `X-API-Key` per
request. Past the cap, a brand-new identity fails open (proxied,
unlimited) rather than being tracked. M3 replaces the map with TTL'd
Redis keys, which age out on their own and remove the need for the cap.

## License

[MIT](LICENSE)
