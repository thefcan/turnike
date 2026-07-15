# PROGRESS

Session state for the multi-session build. Read this first at session start;
update it before session end. Milestone definitions live in the build plan
(see CLAUDE.md for ground rules).

## Milestones

- [x] Setup — repo created (thefcan/turnike, MIT, About + topics), agent
      workflow scaffolded (CLAUDE.md, advisor agent, ship-milestone +
      bench-report skills)
- [ ] M0 — Skeleton: module layout, YAML config + tests, Makefile,
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

Start **M0**: go module (`github.com/thefcan/turnike`), layout
(cmd/gateway, internal/{proxy,limiter,config,metrics}, mock/), YAML config
package with table-driven tests, Makefile, .golangci.yml (v2 format),
GitHub Actions CI, Dockerfile (gateway + mock targets), docker-compose.dev.yml
(redis + mock upstream), config.example.yaml.

## Decisions

### Settled
- Repo name: **turnike** (user).
- YAML parsing: **gopkg.in/yaml.v3** (user approved 2026-07-15).
- Local checkout: `~/turnike`.
- Redis key prefix will be `turnike:{algo}:{key}` (renamed from the plan's
  `rategate:` prefix along with the repo).

### Open (waiting on user)
- M7 deploy (Fly.io + managed Redis): do it at all? Accounts/cost are the
  user's call — ask before starting.

## Session log

- **2026-07-15** — Session 1: pre-flight checks (go 1.26.3, gh, docker,
  golangci-lint 2.12.2 all OK), repo created and pushed, agent workflow
  scaffolded. Note: local golangci-lint is v2 → `.golangci.yml` uses the
  `version: "2"` config format.
