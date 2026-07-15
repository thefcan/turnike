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
- [ ] M1 — Reverse proxy core: ReverseProxy + route table, X-API-Key identity,
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

Start **M1** (reverse proxy core): httputil.ReverseProxy with the route
table from internal/config (longest prefix wins — already decided and
documented), client identity = X-API-Key with client-IP fallback, /healthz +
/readyz, request-ID structured logging (slog), graceful shutdown, timeouts
everywhere (server Read/Write/Idle + per-upstream). Tests: routing, unknown
route → 404, header passthrough, dead upstream → 502. Replace the 501 stub
handler in cmd/gateway/main.go.

Advisor note carried into M1: decide **segment-boundary prefix matching**
(`/api` matches `/api` and `/api/...`, not `/apiv2`) when the route table
lands, and document it on the Route type — the uniqueness check already
treats `/api` == `/api/`.

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
