# turnike

**turnike** — a turnstile for your APIs. A distributed rate limiter & API
gateway in Go, built in public. Hand-implemented token bucket, sliding window
and fixed window algorithms with atomic shared state via Redis+Lua; on the
roadmap: burst-load benchmarks against a multi-replica setup.

> 🚧 Work in progress. The full design-doc README (problem statement,
> architecture, measured trade-offs) lands with milestone M6 — see
> [PROGRESS.md](PROGRESS.md) for current status.

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
different limit for specific API keys. All three run behind one `Limiter`
interface with two backends — **memory** (per-instance state) and
**redis** (state shared by every instance, see below) — and the request
path is identical either way.

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
unlimited) rather than being tracked. The redis backend replaces the map
with TTL'd keys, which age out on their own and remove the need for the
cap.

## Distributed rate limiting (Redis + Lua)

With `limiter.backend: redis`, every gateway instance shares one set of
counters: N replicas enforce one quota instead of N quotas that happen
to share a config file.

### The bypass, measured

`make demo` (or `./scripts/demo_bypass.sh`) stands up nginx
round-robining 3 gateway replicas over one redis and drives 150
sequential requests with a single identity at a route limited to
`fixed_window rate=30 window=1h`. The identity travels as `X-API-Key` —
behind a load balancer every client collapses to the LB's address, so
the key header is what keeps identities apart. Same topology twice; the
only difference between the runs is the gateway config's `limiter`
block:

| `limiter.backend` | quota | requests | admitted (200) | denied (429) |
|---|---:|---:|---:|---:|
| `memory` | 30 | 150 | **90** | 60 |
| `redis` | 30 | 150 | **30** | 120 |

With per-instance memory the mechanism admits up to replicas × rate; the
measured run hit that bound exactly — round-robin split the requests
50/50/50 and each replica granted its own 30, visible in the raw output
as `x-ratelimit-remaining` counting down in three interleaved sequences
(29, 29, 29, 28, 28, 28, …). With redis, request 30 was the last one
through (`remaining=0`) and request 31 got 429. Raw outputs:
[`bench/demo_bypass_memory.txt`](bench/demo_bypass_memory.txt),
[`bench/demo_bypass_redis.txt`](bench/demo_bypass_redis.txt); tables and
notes in [`bench/REPORT.md`](bench/REPORT.md).

The demo's redis config pins `on_error: fail_closed` rather than the
`degrade` default: under degrade an unreachable redis silently falls
back to per-instance counting — faking the very bypass the demo exists
to disprove. The policy changes failure behavior only; the
exactly-the-quota result does not depend on it.

### Why not GET-then-SET

A rate-limit decision is a read-check-write. Done as separate commands,
two gateways interleave: at rate 10 with the count at 9, A reads 9, B
reads 9, both conclude "under limit", both write — 11 admitted. Every
non-atomic implementation has this window, and adding instances widens
it. turnike runs the whole check-and-consume as **one Lua script per
decision**: redis executes scripts serially, so the read, the verdict
and the write are one atomic step — and one round trip. A hammer test
per algorithm drives 4 clients × 100 goroutines at one key and asserts
*exactly* its quota admitted — `rate` for the window algorithms, `burst`
for token_bucket.

### One clock: redis TIME

The scripts take time from `redis.call('TIME')`, in integer
microseconds. No gateway clock participates in any decision, so
instances need not agree on wall time — a drifting replica cannot
stretch or shrink anyone's window. Redis 7 replicates scripts by
effects, which is what makes writing after the non-deterministic TIME
call legal. Two footnotes, both benign and both documented in the
script headers: the fixed_window grid anchors at the Unix epoch (the
in-memory backend anchors at Go's zero time — the backends never share
state), and because TIME is gettimeofday, not monotonic, fixed_window
keeps its stored window when TIME steps backward instead of handing out
a fresh quota.

### EVALSHA and restarts

Scripts are addressed by SHA-1 (`EVALSHA`). Whenever the script cache is
empty — first boot, a redis restart, a `SCRIPT FLUSH` — the client falls
back to `EVAL`, which executes *and re-caches* the script, so limiting
self-heals on the very next decision; an integration test flushes the
cache mid-run and asserts exactly `rate` admissions across the flush.
Boot additionally does a best-effort `SCRIPT LOAD`, surfacing Lua syntax
errors immediately instead of on the first request.

### Every key expires

| algorithm | state | TTL |
|---|---|---|
| fixed_window | hash `{ws, count}` | window end |
| token_bucket | hash `{tokens, last_us}` | time to refill to full (full ≡ fresh key) |
| sliding_window | zset of accept times | one window |

Identity is unauthenticated client input, so keys must age out on their
own — the structural fix for what the in-memory backend's 100k cap
patches. sliding_window also trims (`ZREMRANGEBYSCORE`) before every
decision, and its members carry a per-call nonce: TIME has µs
resolution, so two same-µs accepts are real and must not collapse into
one zset entry.

### When redis is down: `on_error`

Every redis call runs through a small circuit breaker — 5 consecutive
failures open it, one probe per 1s cooldown decides recovery — so an
outage costs each instance roughly one probing call per cooldown (plus
that probe's timeout), not one failed call per request. The `on_error`
policy says what an unanswerable decision means:

| `on_error` | requests | rate-limit headers | `/readyz` | over-admission while down |
|---|---|---|---|---|
| `fail_open` | proxied, unlimited | none | 200 | unbounded |
| `fail_closed` | 503 + `Retry-After` | none | **503** | zero |
| `degrade` (default) | limited per instance, in memory | real, instance-local | 200 | ≤ instances × limit |

`degrade` falls back to the same in-memory limiter the memory backend
uses, so the upstream stays approximately protected *and* available;
recovery can double-count briefly (bounded by one window) when redis
state expired during the outage. `fail_closed` is for enforcement
semantics (quotas, billing): nothing may slip through, at the price of
turning a redis outage into a request outage. It is also the only
policy whose redis ping gates `/readyz` — under the other two the
instance is still doing useful work, and redis is a *shared*
dependency: readiness-failing every instance at once would drain the
whole pool and manufacture the very outage those policies were chosen
to survive. One accepted edge: under fail_closed a fresh PONG can
advertise an instance up to one cooldown before the breaker's next
probe closes the circuit.

Scope note: single-node redis (one `addr`); the sliding-window script
would need hash-tagged keys under redis cluster. The multi-replica
bypass is measured (see [`bench/REPORT.md`](bench/REPORT.md)); latency
and throughput numbers wait for M6's load runs.

## License

[MIT](LICENSE)
