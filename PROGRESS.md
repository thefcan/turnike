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
- [x] M3 — Redis + Lua: one atomic script per algorithm (EVALSHA + EVAL
      self-heal), Redis TIME µs clock, TTL'd keys, on_error policy
      (fail_open/fail_closed/degrade) + circuit breaker, per-algorithm
      hammers proving exact quotas across 4 clients × 100 goroutines
- [x] M4 — Multi-instance proof: demo compose (nginx round-robin → 3
      replicas over one shared redis), scripts/demo_bypass.sh with
      integrity/epoch/freshness guards, recorded runs in bench/ (memory
      90/150 admitted vs redis exactly 30/150), README comparison table
- [x] M5 — Observability: prometheus/client_golang (user-approved) on a
      bare registry — exactly 4 families (requests by route+decision,
      duration histogram, breaker gauge, one-hot backend gauge),
      /metrics as a reserved path, prometheus + grafana provisioned
      into the demo compose (committed dashboard JSON, uid-pinned
      datasource), DEMO_BACKEND=degrade drill arm
- [ ] M6 — Benchmarks + README-as-design-doc, demo-video shot list
- [ ] M7 — OPTIONAL deploy (Fly.io) — requires user approval first

## Next action

Start **M6**: benchmarks + README-as-design-doc + demo-video shot
list. bench/REPORT.md's "Latency / throughput" section is the
placeholder to fill — load runs against the demo compose (the new
turnike_request_duration_seconds histogram gives the gateway's own
p50/p95/p99 to put next to the load generator's view; numbers only
from recorded runs per CLAUDE.md). The README then gets its design-doc
pass (M5 added ## Observability; keep the rewrite modest) and the
demo-video shot list should include the grafana degrade drill —
it is the most demoable thing the repo has.

Advisor backlog: empty — all 10 M5 design pre-review findings were
folded in before implementation, and all 5 ship-review findings
(canceled-caller degrade blip, backend-gauge mutex + HELP caveats,
drill quota vs loop math, dashboard memory-arm mislabel, label-test
overclaim) landed in the fix commit.

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
  Past the cap a brand-new key's request is allowed — it fails open at
  the gateway (logged, no X-RateLimit-* headers) — the key is not
  tracked, and nothing is evicted; already-tracked keys keep limiting
  normally. No background reaper. M3's Redis TTLs remove the need for
  the cap entirely rather than replacing it with a smarter one.
- `tokenBucket`'s admit check tolerates `1 - 1e-9` instead of a strict
  `>= 1`: float64 refill arithmetic can land exactly-1.0-in-real-math
  one ULP under for rate/window ratios that don't divide evenly (e.g.
  1/3) — reproduced deterministically, regression-tested.
- Client-cancelled requests log status **499** (nginx convention), not
  the recorder's default 200 — fixed in M2 alongside the rate-limit
  wiring since both touch the same request path.
- Failure policy (M3, **user-picked in plan review**):
  `limiter.redis.on_error` ∈ fail_open | fail_closed | **degrade
  (default)**. degrade = a MemoryLimiter fallback built once at boot
  inside RedisLimiter answers whenever redis can't (real headers,
  instance-local quota, over-admission ≤ instances × limit, ≤1 window
  of double-counting on recovery); fail_closed = gateway 503 +
  Retry-After derived from the exported `limiter.BreakerCooldown` (no
  X-RateLimit headers — no quota state to report); fail_open = M2's
  behavior made explicit. The policy reaches the gateway **only under
  the redis backend** — memory's at-capacity error always fails open
  (advisor ship-review catch, pinned by a handler test).
- Circuit breaker (M3): hand-rolled, consts not knobs — 5 consecutive
  failures open it, `BreakerCooldown` = 1s to a single half-open probe.
  `context.Canceled` is neutral (client hung up ≠ redis down); a
  canceled probe re-opens with `openedAt` untouched so the next caller
  re-probes immediately (no wedge); the open-transition log carries the
  underlying error so a script bug can't masquerade as an outage under
  degrade. `Ping` (readyz) and boot script loads bypass the breaker:
  probes must report ground truth and must not hold the circuit open.
- readyz (M3, advisor-ruled): the redis ping is registered as a
  ReadyCheck **only under fail_closed** — redis is a shared dependency,
  so an unconditional 503 would drain every instance from the LB at
  once, manufacturing the outage fail_open/degrade exist to survive.
  Pinned by TestReadyzReflectsPolicyWhenRedisDown. /healthz + /readyz
  are GET/HEAD-only (405 + Allow otherwise), each check runs under a 1s
  const budget, and the not-ready body is generic with the reason
  logged (the error would leak the redis addr on a public listener).
- Redis time (M3): scripts source time exclusively from `TIME` in
  integer µs (exact in Lua float64 to 2^53). fixed_window's grid
  anchors at the Unix epoch vs the memory backend's Go-zero-time
  `Truncate` — benign, backends never share state — and because TIME is
  gettimeofday (not monotonic) the script keeps the stored window when
  TIME steps backward instead of granting a fresh quota. Every stored
  number is `string.format`ted (`%d` / `%.17g`); Lua's implicit
  `tostring` (%.14g) would corrupt µs timestamps and token floats.
- sliding_window member uniqueness (M3): score = accept-time µs, member
  = `{µs}-{per-call 64-bit rand/v2 nonce}` passed as ARGV. Two same-µs
  accepts are real (scripts run back-to-back) and identical members
  would collapse → silent over-admission; the hammer asserts ZCARD ==
  rate to falsify. An INCR seq key (second key + TTL lifecycle) and
  in-script math.random (redis seeds it identically per invocation —
  same-µs callers collide by construction) were both rejected.
- go-redis client (M3): Dial/Read/WriteTimeout 1s consts, `MaxRetries:
  -1` — the breaker owns retry behavior; hidden client retries would
  mask the failures it counts. Construction never fails on unreachable
  redis (eager SCRIPT LOAD is best-effort): the process boots under its
  failure policy instead of crash-looping; under fail_closed + redis
  never reachable that means serving 503s and staying not-ready — by
  design.
- Integration test windows (M3): only 1s / 1m / 1h — they divide the
  62,135,596,800s Go-zero↔epoch offset so both grids coincide — and
  hammers use hour-scale windows/refills so "exactly quota" can't be
  broken by a mid-hammer boundary or refill; the fixed hammer
  additionally waits out a start within 5s of the hour boundary on
  redis's own clock. Suite is REDIS_ADDR-gated: unset skips, set but
  unreachable **fails** (CI can't silently skip).
- Redis cluster is a non-goal (single `addr`; the sliding-window script
  would need hash-tagged keys) — documented in the README scope note.
- Advisor reviews run pinned to the fable model (user instruction,
  2026-07-15) — `.claude/agents/advisor.md` frontmatter.
- Demo compose isolation (M4): top-level `name: turnike-demo` — the dev
  compose defaults to the directory-derived project name and both
  define redis+mock, so without it the demo's up/down would recreate or
  destroy the possibly-in-use dev containers. Only nginx publishes a
  host port (8090; dev owns 6379/9000). `build:` lives on gateway-1
  only; gateway-2/3 reuse the shared `turnike-gateway:demo` tag.
- Demo failure policy (M4): the redis arm pins `on_error: fail_closed`,
  not the degrade default — under degrade an unreachable redis silently
  falls back to per-instance counting and would fake the very bypass
  the demo disproves; fail_closed turns redis trouble into 503s (script
  aborts) and is the one policy that puts the redis ping on /readyz, so
  `up -d --wait` proves connectivity before a request fires. Failure
  behavior only: the exactly-the-quota result does not depend on it.
- Demo LB determinism (M4): `worker_processes 1` (OSS nginx keeps
  round-robin state per worker), `max_fails=0`, `proxy_next_upstream
  off` — a broken replica must surface as failed requests, never as
  silent redistribution; `add_header X-Demo-Upstream $upstream_addr
  always` (`always` because the default code list excludes 4xx and
  denial attribution would vanish from 429s).
- Demo window (M4): `fixed_window rate=30 window=1h` — 1h is in the
  M3-blessed set where redis's epoch grid and memory's Go-zero grid
  coincide, and the script waits out the top of the hour on redis's own
  clock (the deciding clock; the Docker VM clock can drift from the
  host's on macOS) so an arm can never straddle a window rollover.
- demo_bypass.sh integrity (M4): assertions gate publication but check
  invariants only — published numbers are whatever was measured. Redis
  arm must admit exactly rate (M3's atomicity property, distribution-
  independent); memory arm >rate and ≤ replicas×rate (exact 3× is an LB
  property, not a limiter property — the recorded run did hit 90/150);
  other==0; exactly 3 distinct upstreams; freshness canary (max
  remaining seen == rate−1); measured line count == configured request
  count. Every replica's boot log must show the intended
  `limiter_backend` (a mis-set DEMO_BACKEND cannot mislabel an arm);
  after the memory arm redis must hold no `turnike:*` keys; a failed
  run appends `# RUN INVALID` and renames its raw file `*.failed`; the
  commit stamp is captured once pre-run (arm 1 regenerating tracked
  bench files would otherwise mislabel arm 2 as `-dirty`).

- Metrics set (M5, user-pinned + advisor-shaped): exactly four families
  on a bare `prometheus.NewRegistry` (no Go/process collectors),
  namespace `turnike_`, instance-scoped handle constructor-threaded
  from cmd/gateway (no globals). `requests_total{route, decision}`
  with decision ∈ {allow, deny, degrade_allow, degrade_deny, degrade}:
  allow/deny = the configured backend's own verdict; degrade_* = the
  degrade fallback's verdict (advisor pre-review: a single bare
  `degrade` would erase the 429 rate during an outage); bare degrade =
  no verdict existed (fail_open pass-through, fail_closed 503, memory
  at-capacity fail-open) — the label family is wider than the
  `on_error: degrade` policy name, documented in HELP + README.
  Identity is NEVER a label (unauthenticated client input — the
  maxKeys threat model at the TSDB layer, pinned by a scrape-sweep
  test); route label only from the config table; 404s and reserved
  paths uncounted; route×decision series pre-materialized so rate()
  sees them from the first scrape.
- Degrade seam (M5): `Decision.Degraded bool`, set only on the
  fallback-success return, observability-only. Rejected alternatives:
  an exported breaker-state accessor (the fallback answers on the 4
  pre-trip failures too — breaker closed while degrading) and
  limiter-side counting (no route label there). `context.Canceled`
  skips the fallback entirely (ship review): the breaker holds it
  neutral and a fallback answer would paint a degrade blip + backend
  flip for a client hang-up on healthy redis.
- Limiter instrumentation (M5): `limiter.Instruments` carries two
  small structural interfaces (breaker gauge `Set(float64)`, backend
  `SetActiveBackend(string)`) so internal/limiter imports neither
  metrics nor client_golang; zero value = no-ops. The breaker writes
  state through one `setState` choke point under its mutex (gauge
  exports the breaker's own 0/1/2 consts, materialized at
  construction, constant 0 under the memory backend — HELP says so).
  `limiter_backend{backend}` one-hot: set on who answered (redis
  script success / fallback answer); error paths without a fallback
  leave it alone (under fail_open/fail_closed nothing else answers —
  breaker_state carries those outages); flips serialized by a mutex
  (last decision literally wins; a scrape may catch one mid-flip);
  redis construction marks redis active, and building the degrade
  fallback must not flip it (pinned).
- Histogram (M5): `request_duration_seconds` label-less, all outcomes
  (denials observed too — quantiles move with the deny mix; HELP,
  README and the panel say so). Buckets = [.5ms, 1ms, 2.5ms] +
  DefBuckets, worded as expectation (loopback answers would pile into
  DefBuckets' 5ms floor) — measured latency numbers wait for M6.
  Observed in the middleware's deferred block gated on the same
  routePrefix the counter requires → observations == increments,
  pinned across allow/429/fail_closed-503/404/reserved.
- /metrics (M5): third reserved path, GET/HEAD-only 405+Allow
  (promhttp answers POST if left alone — the guard is load-bearing),
  outside the access log and the quota, zero config surface
  (KnownFields(true) untouched). Data-plane /metrics + LB forwarding
  is a documented demo simplification (README scope note: production
  binds a separate admin listener).
- Demo observability (M5): prom/prometheus:v3.5.5 (LTS) +
  grafana/grafana:12.4.5 in the demo compose, always on (user pin:
  plain `docker compose up` gives everyone the same panels — no
  profiles); provisioning via ro bind mounts under demo/ only (the
  `down --volumes` teardown wiping grafana state between runs is the
  same-panels guarantee); datasource uid "prometheus" pinned in both
  the provisioning yml and every panel. Grafana anonymous Viewer on
  host **3300** (native 3000 is the most commonly dev-occupied port
  and a clash would wedge `up --wait` for measured runs), prometheus
  9090 — amends M4's only-nginx-publishes rule (its rationale was
  dev-compose clashes; these clash with nothing). scrape_interval 1s
  (breaker cooldown is 1s; half-open is sub-scrape and pinned by unit
  tests — the panel description says so). No healthchecks on the pair
  so demo_bypass.sh's `up -d --wait` cannot wedge; `make demo`
  re-verified green with both new services up.
- Degrade drill arm (M5): demo/gateway-degrade.yaml
  (DEMO_BACKEND=degrade, on_error degrade, fixed_window **2**/1s) —
  M4's redis arm deliberately pins fail_closed, so no existing arm
  could show the flip; demo_bypass.sh still drives only memory|redis.
  Quota 2/s + an 8-way parallel curl hammer keep a deny band visible
  in BOTH phases (measured: healthy allow +4/deny 294; degraded
  degrade_allow +6/degrade_deny 294) — a sequential curl loop tops out
  below the replicas' aggregate fallback quota and hides the degraded
  deny band entirely (the ship review caught the original rate-5 +
  paced-loop combination making the claim mathematically impossible).
  Field note confirmed live: until each replica's breaker trips, every
  decision burns the 1s client timeout (Docker Desktop turns
  stopped-container dials into ~1s hangs), so the first post-stop
  seconds crawl at ~1 rps/replica and a short loop never reaches the
  5-failure threshold; the README warns about the spike and calls it
  the breaker earning its keep.

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
- **2026-07-15** — Session 4: M3 shipped in 14 commits. Advisor consulted
  three times: (1) backlog pre-review — #7 folded into M3 (the drain
  test forced run()'s net.Listen/serve split, which M3 needed anyway
  for the redis Close-after-Shutdown ordering), #10 shaped
  (GET/HEAD-only + 405, 1s per-check timeout, policy-aware readyz);
  (2) design pre-review of the Lua/breaker/policy plan — SOUND, four
  findings folded in before a line was written (hour-window hammers,
  backward-TIME-step guard, exported BreakerCooldown, ARGV nonce
  replacing the seq-key design); (3) milestone diff review — SHIP with
  four advisory findings, all fixed (memory-backend on_error isolation,
  dead assert, hour-boundary hammer wait, README precision). User
  picked **degrade** as the on_error default in plan review. Verified:
  unit + integration green under `-race` against real redis (hammers
  admit exactly quota from 4 clients × 100 goroutines; SCRIPT FLUSH
  mid-run self-heals with state intact), lint clean at every commit
  boundary, plus a live E2E against docker redis + mock upstream:
  3-allow/1-deny with correct headers, POST /healthz → 405, flush →
  still 429, `docker compose stop redis` → degrade answers with real
  headers while readyz stays 200 and the breaker-open log carries the
  error, restart → probe closes the circuit, fail_closed variant → 503
  + Retry-After: 1 + readyz 503 with a generic body, SIGTERM → clean
  drain (exit 0). Field note: Docker Desktop's port forward turns
  stopped-container dials into ~1s timeouts rather than refusals — the
  1s client budgets + breaker absorbed it as designed.
- **2026-07-17** — Session 6: M5 shipped in 8 commits
  (client_golang v1.23.2 user-approved in plan review). Advisor
  consulted twice, both rounds FIX FIRST, everything folded in: (1)
  design pre-review, 10 findings before a line was written — decision
  label split (degrade → degrade_allow/degrade_deny + bare degrade),
  limiter.Instruments structural interfaces instead of a metrics
  import, one-hot weakened to last-decision-wins + boot redis=1,
  bucket comment as expectation not measurement, histogram documented
  all-outcomes, grafana host 3300 over clash-prone 3000, 1s scrape for
  the 1s breaker, nil-gauge no-op so existing tests survive, histogram
  asserts on the 429/503 paths + a sequence-recording gauge fake,
  admin-listener scope note; (2) ship review, 5 findings — the
  canceled-caller fake-degrade blip (only behavior change: neutral
  errors skip the fallback), SetActiveBackend mutex + HELP caveats,
  the drill's rate-5-vs-paced-loop math making its own claim
  impossible (now 2/s + a parallel hammer, re-measured), the
  dashboard's "memory (degraded)" mislabel on the memory arm, and the
  childless-vec blind spot in the label test (cardinality pin moved to
  a wire-format scrape sweep). Verified live: binary smoke (4 families
  only, POST 405, scrape unlogged/uncounted), single-instance degrade
  drill against dev redis (3×200+5×429 via redis; stop → 7
  degrade_allow + 3 degrade_deny, breaker-open log carries the dial
  error; start → probe closes), full demo stack (3 targets up,
  provisioned dashboard on grafana 12.4.5 by uid, stop redis → all
  three replicas flip breaker=Open/backend=memory, hammer paints
  degrade_deny 294; recovery closes circuits one probe-carrying
  request per replica), `make demo` end-to-end green with prometheus +
  grafana aboard (memory 90/150, redis exactly 30/150, no --wait
  wedge, `git restore bench/` after — recorded M4 numbers stand),
  `go test -race` + lint green at every commit boundary, `go mod
  tidy` diff-free. Field note: a sequential curl loop through the LB
  tops out at a few rps (curl spawn + docker forward), and until the
  breakers trip each unanswered decision burns the full 1s client
  timeout — both discovered by the ship-review math and confirmed by
  measurement before the README's drill instructions were written.
- **2026-07-17** — Session 5: M4 shipped in 7 commits, zero Go changes
  (compose + nginx + bash + recorded measurements + docs). Advisor
  consulted twice: (1) design pre-review — SOUND, six findings folded
  in before a line was written (turnike-demo project isolation,
  boot-log integrity guard, epoch-boundary guard as mandatory,
  assert-invariants/report-measurements policy, no-silent-healing nginx
  knobs, dir-mount for the demo configs); (2) milestone ship review —
  FIX FIRST (1 moderate + 3 low), all fixed: leftover-stack reuse
  (down-before-build + freshness canary), assertion-failed runs marked
  RUN INVALID and renamed `*.failed`, footer upstreams resolved to
  gateway-N names, `total=` counts data lines; plus one self-caught fix
  (commit stamp captured once pre-run). Recorded run — three executions,
  identical numbers: memory 90/150 admitted (strict 50/50/50 RR split,
  each replica granting its own 30, three interleaved 29,29,29,28,…
  countdowns, no `turnike:*` keys in redis afterwards), redis exactly
  30/150 (seq 30 → 200 with remaining=0, seq 31 → 429), other=0 both
  arms. Verified: dev-compose redis untouched by demo up/down cycles,
  no leftover turnike-demo containers/volumes, `go test -race` + lint
  green at every commit boundary. README gained the measured comparison
  table; bench/REPORT.md cites the raw files line by line.
