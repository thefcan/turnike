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
- [ ] M2 — Algorithms in-memory: fixed window, token bucket, sliding window log;
      Limiter interface; 429 + Retry-After + X-RateLimit-* headers
- [ ] M3 — Redis + Lua: one script per algorithm (EVALSHA), Redis TIME,
      TTL'd keys, fail-open/fail-closed + circuit breaker, hammer test
- [ ] M4 — Multi-instance proof: nginx LB → 3 replicas demo compose,
      scripts/demo_bypass.sh, bench/ raw outputs, README comparison table
- [ ] M5 — Observability: Prometheus metrics, /metrics, Grafana dashboard
- [ ] M6 — Benchmarks + README-as-design-doc, demo-video shot list
- [ ] M7 — OPTIONAL deploy (Fly.io) — requires user approval first

## Next action

Start **M2** (algorithms in-memory): fixed window, token bucket, sliding
window log behind a `Limiter` interface in internal/limiter. Pure,
clock-injected logic (deterministic tests — project rule). On deny: 429 +
`Retry-After` + `X-RateLimit-Limit/Remaining/Reset` headers, wired into the
gateway between identity extraction and proxying (`Identity.Value` is the
raw key for `Route.LimitFor`; `Identity.String()` is the fingerprinted
limiter/log key).

Advisor backlog carried out of M1 (fix in M1.x/M2 as they become relevant):
- Client-gone requests are logged as 200 — log ctx error or use the 499
  convention.
- README should state the "turnike runs at the edge" assumption behind
  not trusting XFF; consider a `trusted_proxies` knob later.
- Graceful-shutdown test doesn't prove draining (only that run() returns
  nil after cancel).
- 502 path only tested via dial failure; add a response-header-timeout
  test.
- /healthz//readyz answer every HTTP method; readyz checks have no
  per-check timeout (resolve before M3's Redis ping).

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
