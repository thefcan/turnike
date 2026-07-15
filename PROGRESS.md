# PROGRESS

Session state for the multi-session build. Read this first at session start;
update it before session end. Milestone definitions live in the build plan
(see CLAUDE.md for ground rules).

## Milestones

- [x] Setup — repo created (thefcan/turnike, MIT, About + topics), agent
      workflow scaffolded (CLAUDE.md, advisor agent, ship-milestone +
      bench-report skills)
- [x] M0 — Skeleton: module layout, YAML config + tests, Makefile,
      golangci-lint, CI (lint + test -race), docker-compose.dev.yml, mock upstream
- [x] M1 — Reverse proxy core: ReverseProxy + route table, X-API-Key identity,
      /healthz /readyz, slog request-ID logging, graceful shutdown, timeouts
- [x] M2 — Algorithms in-memory: fixed window, token bucket, sliding window log;
      Limiter interface; 429 + Retry-After + X-RateLimit-* headers
- [ ] M3 — Redis + Lua: one script per algorithm (EVALSHA), Redis TIME,
      TTL'd keys, fail-open/fail-closed + circuit breaker, hammer test
- [ ] M4 — Multi-instance proof: nginx LB → 3 replicas demo compose,
      scripts/demo_bypass.sh, bench/ raw outputs, README comparison table
- [ ] M5 — Observability: Prometheus metrics, /metrics, Grafana dashboard
- [ ] M6 — Benchmarks + README-as-design-doc, demo-video shot list
- [ ] M7 — OPTIONAL deploy (Fly.io) — requires user approval first

## Next action

Start **M3** (Redis + Lua): one script per algorithm (EVALSHA), sourcing
time from Redis TIME (not the node's clock) so multiple instances agree,
TTL'd keys (removes the need for MemoryLimiter's maxKeys cap — see
Decisions), fail-open/fail-closed as an explicit configurable policy
(generalizing M2's fixed fail-open), circuit breaker around Redis calls,
hammer test proving atomicity under real concurrent instances. The
`Limiter` interface (internal/limiter/limiter.go) and the `Decision`
shape it returns were designed for this — a `RedisLimiter` implementing
`Allow(ctx, key, limit) (Decision, error)` is a drop-in for
`MemoryLimiter`, selected via `limiter.New`'s existing `cfg.Limiter.Backend`
switch (currently errors "lands in M3" on `redis`). Redis key scheme:
`turnike:{algo}:{key}` (settled since M0) — note the gateway's own key
already looks like `{route_prefix}:{algo}:{identity}` end to end via
`MemoryLimiter`'s `algorithm+":"+key` state key, so EVALSHA scripts can
reuse that shape directly.

Advisor backlog still open (not M2-coupled, unresolved):
- Graceful-shutdown test doesn't prove draining (only that run() returns
  nil after cancel).
- /healthz//readyz answer every HTTP method; readyz checks have no
  per-check timeout (resolve when M3's Redis ping lands on /readyz).

## Decisions

### Settled
- Repo name: **turnike** (user).
- YAML parsing: **gopkg.in/yaml.v3** (user approved 2026-07-15).
- Local checkout: `~/turnike`.
- Redis key prefix will be `turnike:{algo}:{key}` (renamed from the plan's
  `rategate:` prefix along with the repo).
- Limit semantics (advisor-reviewed, M0): **rate = requests per window for
  every algorithm**; for token_bucket, window is the refill interval and
  defaults to 1s; burst (bucket capacity) is token_bucket-only and rejected
  elsewhere. Per-key overrides merge field-wise over the base limit, except
  an override that switches algorithm must be self-contained.
- Routing: **longest matching prefix wins** (documented in M0, implemented
  in M1); prefixes differing only by a trailing slash are rejected as
  duplicates.
- Toolchain pinned to **Go 1.26** across go.mod, CI and Dockerfile so the
  race-tested toolchain is the shipped one.
- Routing (M1, advisor-reviewed): **segment-boundary** longest-prefix —
  `/api` matches `/api` and `/api/...`, never `/apiv2`; `/api/v1` never
  captures `/api/v1beta/x`. Matching runs on the cleaned escaped path;
  the URL is forwarded unchanged (no prefix stripping). Paths with dot
  segments (plain or percent-encoded) are rejected with 404 so the
  matched route and the upstream's resolved path cannot diverge.
  Documented on config.Route.
- Identity (M1): `X-API-Key` else client IP; XFF not trusted. Log/Redis
  form is `key:<sha256[:8] fingerprint>` / `ip:<addr>` — the raw key
  never appears in logs and is used only for KeyOverrides lookups.
- `/healthz` and `/readyz` are reserved paths served ahead of the route
  table, outside the access log; readiness takes pluggable checks (M3
  adds the Redis ping).
- Timeout defaults (M1): server read-header 5s / read 30s / write 60s /
  idle 120s / shutdown 10s; upstream dial 5s / response-header 10s. One
  shared transport (connection pool) across routes. Host header is
  rewritten to the upstream host (original in X-Forwarded-Host).
- Limiter shape (M2, Redis-ready): `Limiter.Allow(ctx, key, limit)
  (Decision, error)`; `Decision{Allowed, Limit, Remaining, Reset,
  RetryAfter}`. `Reset` means "back to a clean slate" for every
  algorithm (fixed_window: window end; token_bucket: refilled to burst;
  sliding_window: the *newest* counted entry's expiry, i.e. the whole
  log clear — not the *oldest* entry's, which is a different question
  RetryAfter answers instead: "when does the next request succeed".
  Advisor caught the two being conflated in the first sliding_window
  draft). `Clock` is injected into `MemoryLimiter` only, not the
  interface — M3's Redis backend sources time from Redis TIME instead.
- Gateway rate-limit key: `entry.Prefix + ":" + id.String()`
  (route-namespaced, fingerprinted identity); `MemoryLimiter` further
  isolates by algorithm (`limit.Algorithm + ":" + key`) so a
  key_overrides algorithm switch never reads stale state. `Route.LimitFor`
  is called with the *raw* identity (`id.Value`), never the fingerprint —
  key_overrides is keyed by literal API key.
- Headers (M2): `X-RateLimit-Limit/Remaining/Reset` set on every
  matched-route response (allowed or denied), before proxying, relying
  on `httputil.ReverseProxy` merging upstream headers additively
  (`Header.Add`, never `Set`) rather than clearing what the gateway set —
  confirmed against the stdlib source, not just assumed. `Retry-After`
  only on 429, ceil'd to whole seconds (never advise less wait than
  actual). Upstreams must not set their own `X-RateLimit-*`/`Retry-After` —
  the gateway doesn't strip them, so both values would appear.
- `MemoryLimiter.state` is capped at 100,000 distinct (algorithm:key)
  entries (`maxKeys` in memory.go) — identity is unauthenticated
  (`X-API-Key` is free-form client input), so an unbounded map would
  let any caller inflate gateway memory just by varying the header.
  Past the cap, a new key fails open rather than being tracked; no
  background reaper. M3's Redis TTLs remove the need for the cap
  entirely rather than replacing it with a smarter one.
- `tokenBucket`'s admit check tolerates `1 - 1e-9` instead of a strict
  `>= 1`: float64 refill arithmetic can land exactly-1.0-in-real-math
  one ULP under for rate/window ratios that don't divide evenly (e.g.
  1/3) — reproduced deterministically, regression-tested.
- Client-cancelled requests log status **499** (nginx convention), not
  the recorder's default 200 — fixed in M2 alongside the rate-limit
  wiring since both touch the same request path.

### Open (waiting on user)
- M7 deploy (Fly.io + managed Redis): do it at all? Accounts/cost are the
  user's call — ask before starting.

## Session log

- **2026-07-15** — Session 1: pre-flight checks (go 1.26.3, gh, docker,
  golangci-lint 2.12.2 all OK), repo created and pushed, agent workflow
  scaffolded. Note: local golangci-lint is v2 → `.golangci.yml` uses the
  `version: "2"` config format. M0 shipped: config package (parse +
  validate + per-key override merge, 20+ table-driven cases), gateway/mock
  binaries, Makefile, CI (lint + race tests + tidy check), Dockerfile,
  dev compose (verified: mock echoes JSON, redis PONGs). Advisor review
  round 1 → FIX FIRST (8 findings), all fixed (limit semantics rework,
  multi-doc YAML rejection, prefix-collision check, Go version alignment,
  non-root Docker USER, CI tidy gate, README claim reword).
- **2026-07-15** — Session 2: M1 shipped in 8 commits (config timeouts,
  route table, identity, middleware, gateway, wiring, advisor fixes,
  docs). E2E-verified against the mock upstream: routing + passthrough +
  X-Forwarded-*, /apiv2 → 404, dead upstream → 502 with request-ID
  correlated error log, SIGTERM drain + clean exit, traversal (plain and
  %2e%2e) → 404, fingerprinted key in access log. Advisor review round 1
  → FIX FIRST (10 findings): 1–3 + 6 + 8 fixed (dot-segment rejection,
  deferred access log, key fingerprinting, request-ID charset, honest
  recorder test); 4/5/7/9/10 carried as backlog under "Next action".
- **2026-07-15** — Session 3: advisor pre-review of the M1 backlog decided
  which items to fold into M2 (4 client-gone-as-499, 5 edge/LB doc) vs.
  leave deferred (7, 9 folded in anyway alongside 4 since it's the same
  error path, 10 stays — genuinely M3-coupled). M2 shipped in 6 commits:
  fixed_window, token_bucket, sliding_window (each with a pure,
  clock-injected `step`, over-admission proven under `-race` with real
  concurrent goroutines), gateway wiring (429 + headers, edge/LB doc at
  the key-derivation site), the folded-in 499/timeout-502 fix, docs.
  E2E-verified live against mock: /search/ (sliding, 5/10s) denies on
  the 6th request with correct headers, /auth/ (fixed_window, 3/10s)
  denies on the 4th, token_bucket key_overrides (burst 200 vs 20)
  applies correctly through the real config file. Advisor review round 1
  → FIX FIRST (7 findings), all fixed: MemoryLimiter's state map capped
  at 100k keys (identity is unauthenticated, so it was a live DoS vector,
  not just slow accumulation — sharper than the framing under which the
  no-janitor call was originally made, addressed with a synchronous cap
  instead of revisiting that call); sliding_window's Reset was reporting
  the oldest entry's expiry when Decision's contract means "fully clear"
  (newest entry's expiry) — fixed and regression-tested; token_bucket's
  float64 boundary comparison given an epsilon tolerance after
  deterministically reproducing a real 1-ULP-under-1.0 denial; fail-open
  path given test coverage; two README overclaims corrected. Re-verified
  green (`-race`, lint, live curl) after every fix.
