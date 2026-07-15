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

## License

[MIT](LICENSE)
