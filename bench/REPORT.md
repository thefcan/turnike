# Bench report

Every number below is copied from a raw output file in this directory —
nothing is estimated, averaged away, or extrapolated. Regenerate the
underlying runs with the commands listed per section.

## Multi-instance bypass: memory vs redis (M4)

One identity (`X-API-Key: demo-key`), 150 sequential requests through
nginx round-robining 3 gateway replicas, route `/demo/` limited by
`fixed_window rate=30 window=1h`. The only difference between the two
runs is the `limiter` block of the gateway config. Reproduce with
`./scripts/demo_bypass.sh` (or `make demo`).

Source: `demo_bypass_memory.txt`, `demo_bypass_redis.txt` (both from
commit `47ad2c4`, 2026-07-16, summary lines at the bottom of each file).

| backend | quota | requests | admitted (200) | denied (429) | other |
| ------- | ----: | -------: | -------------: | -----------: | ----: |
| memory  |    30 |      150 |         **90** |           60 |     0 |
| redis   |    30 |      150 |         **30** |          120 |     0 |

Per-replica spread (source: the `# upstream` footer lines of each file):

| replica   | memory: requests | memory: 200 | redis: requests | redis: 200 |
| --------- | ---------------: | ----------: | --------------: | ---------: |
| gateway-1 |               50 |          30 |              50 |         10 |
| gateway-2 |               50 |          30 |              50 |         10 |
| gateway-3 |               50 |          30 |              50 |         10 |

(Footer lines carry the resolved `gateway-N (ip:port)` names, and the
IP→replica mapping is also pinned in each raw file's header. The redis
run's per-replica 200s are 10/10/10 because round-robin interleaves one
shared countdown across the three replicas.)

### Reading

- Under `backend: memory`, each replica admitted its own full quota of
  30 — 90 in total, 3× the configured limit. The bypass is visible line
  by line in `demo_bypass_memory.txt`: `x-ratelimit-remaining` counts
  down as three interleaved sequences (29, 29, 29, 28, 28, 28, …), one
  per replica.
- Under `backend: redis` with the identical topology, exactly 30 of 150
  were admitted: request 30 returned 200 with `remaining=0`, request 31
  returned 429 (`demo_bypass_redis.txt`, lines for seq 30–31). One
  counter, shared by every replica.
- The demo sends `X-API-Key` because behind the LB every client
  collapses to the LB's address — without a key header, all traffic
  would share one IP-derived bucket by design.
- Distribution across replicas was exactly even (50 requests each, both
  runs) — `worker_processes 1` plus sequential requests gives nginx's
  round-robin strict rotation, so the memory run's 90 is the worst-case
  bypass (replicas × rate), not a lucky draw.
- After the memory run, redis held no `turnike:*` keys (negative-proof
  line at the bottom of `demo_bypass_memory.txt`): redis was up the
  whole time, but the counting really happened per instance, in memory.
- The redis run pins `on_error: fail_closed` (not the `degrade`
  default): if redis broke mid-run, degrade would silently fall back to
  per-instance memory counting and fake the very bypass this demo
  disproves. fail_closed turns redis trouble into 503s, which the
  script treats as run-invalidating. It changes failure behavior only —
  the admitted-exactly-30 result does not depend on it.

## Latency & admission under load (M6)

Reproduce with `./scripts/load_test.sh` (or `make load`): a pinned k6
container (`grafana/k6:2.1.0`,
`sha256:65c920dc067d5e2e00befbf982af6ad6ad0117034e8b1c65817c7975c52d4669`)
joined to the demo network drives `http://nginx:8080` directly, so the
Docker Desktop host↔VM port forward is never in the measured path.
Nine runs, each from a fresh stack; raw files in `bench/load/` (a
`.meta.txt` summary + the k6 `.summary.json` + a per-replica
`.metrics.txt` scrape per run), all from commit `f2ba13d`.

**Environment.** One laptop: Docker Desktop VM with 8 vCPU / ~7.6 GiB,
macOS 26.5.2, everything (k6, nginx, 3 gateways, redis, mock,
prometheus, grafana) sharing that VM. **These numbers are for relative
comparison on this rig — algorithm vs algorithm, redis vs memory — not
absolute capacity claims.** Two things sit in the measured path and
are named here rather than hidden: the k6→gateway path crosses nginx
with no upstream keepalive, so each request opens a fresh nginx→gateway
TCP connection (in k6's client-side view, not the gateway histogram's);
and the three replicas contend for the same VM cores as everything
else, which is what the noisy tails below are.

### Sustained allow path — what a decision costs

200 rps constant arrival for 60s per (algorithm × backend), route quota
far above arrival so nothing is denied (~12,000 iterations — 12,001 in
five runs, 12,007 in fixed_window/redis — and 0×429 every run) and the
percentiles measure identity → route → decision → proxy → mock. Two views of the same runs: k6's exact client-side percentiles,
and the gateway's own `turnike_request_duration_seconds` histogram
(three replicas summed), reported as the bucket the quantile falls in
with the `histogram_quantile`-style interpolation in parentheses — the
buckets are coarse (`0.5, 1, 2.5, 5, 10, 25 ms`) so the bound is the
honest statement and the interpolation is a within-bucket guess.

k6 client-side, milliseconds (source: `bench/load/sustained_*_*.summary.json`):

| algorithm | backend | achieved rps | med | p95 | p99 | max |
| --------- | ------- | -----------: | --: | --: | --: | --: |
| fixed_window   | redis  | ~200 | 1.31 | 3.80 |  36.15 | 168.56 |
| token_bucket   | redis  | ~200 | 1.36 | 7.38 |  47.90 | 189.13 |
| sliding_window | redis  | ~200 | 1.37 | 4.81 |  22.25 |  96.61 |
| fixed_window   | memory | ~200 | 1.37 | 2.91 |   9.11 | 165.98 |
| token_bucket   | memory | ~200 | 1.19 | 3.16 |   6.87 | 109.21 |
| sliding_window | memory | ~200 | 1.20 | 3.12 |   5.72 |  43.33 |

Gateway-side histogram, bucket bounds in ms (interp) (source: the
`gateway q=...` lines in each `bench/load/sustained_*_*.meta.txt`, from
the `.metrics.txt` scrape):

| algorithm | backend | p50 | p95 | p99 |
| --------- | ------- | --- | --- | --- |
| fixed_window   | redis  | (0.5, 1] (0.65) | (1, 2.5] (2.12) | (5, 10] (7.46)   |
| token_bucket   | redis  | (0.5, 1] (0.62) | (2.5, 5] (2.53) | (10, 25] (19.51) |
| sliding_window | redis  | (0.5, 1] (0.65) | (1, 2.5] (2.31) | (10, 25] (11.48) |
| fixed_window   | memory | (0, 0.5] (0.32) | (1, 2.5] (1.00) | (1, 2.5] (2.42)  |
| token_bucket   | memory | (0, 0.5] (0.33) | (0.5, 1] (1.00) | (1, 2.5] (2.42)  |
| sliding_window | memory | (0, 0.5] (0.33) | (0.5, 1] (0.99) | (1, 2.5] (2.36)  |

### Admission semantics — the same spike, three answers

~300 rps for 6s (≈1800 requests, one identity) at a route limited to
`rate 50 / 10s`, timed off redis `TIME` so the ~6s spike straddles a
fixed_window epoch-grid boundary; each run's k6 start landed within
20 ms of the target instant (the `scenario_start` line in each
`bench/load/burst_*_redis.meta.txt`). Redis backend, `on_error:
fail_closed`. Source: `bench/load/burst_*_redis.meta.txt`.

| algorithm | sent | admitted (200) | denied (429) | what the number is |
| --------- | ---: | -------------: | -----------: | ------------------ |
| fixed_window   | 1801 | **100** | 1701 | 50 on each side of the window boundary the spike straddles — the 2× edge, `> rate` and `≤ 2×rate` |
| sliding_window | 1800 |  **50** | 1750 | exactly `rate`: a timestamp log has no fixed grid to reset against, and the spike is shorter than the window |
| token_bucket   | 1801 |  **79** | 1722 | the full burst (50) up front, then ~6s of refill at rate/window = 5/s ≈ 30 more (ideal 80) |

### Reading

- **The median is the clean signal; read the redis cost there.** On the
  gateway's own histogram every redis run's median sits in (0.5, 1] ms
  and every memory run's in (0, 0.5] ms — a ~0.3 ms gap (0.65 vs 0.32,
  0.62 vs 0.33, 0.65 vs 0.33 for fixed/token/sliding). That gap, read
  within one view so the mock upstream time cancels, is the price of
  one shared-state redis round trip. It is deliberately not read off
  the k6 column, where the per-request nginx TCP connect adds ~1 ms of
  its own and partly masks it (k6 memory medians are ~1.2–1.4 ms
  against gateway-side ~0.3 ms — that ~1 ms difference is the nginx
  hop, not the limiter).
- **The tails are noise on this rig — don't over-read them.** p99 across
  the six sustained runs scatters from 5.7 to 47.9 ms (k6) with no
  algorithm consistently worst — token_bucket had the lowest redis tail
  in an earlier batch and the highest here. On a single VM where k6 and
  three gateways fight for 8 shared cores that is scheduling jitter, not
  algorithm structure, which is why the gateway quantiles are published
  as bucket bounds rather than a false-precision p99.
- **sliding_window was measured at the small end of its cost.** At a 1s
  window and ~200 rps its zset holds ~200 members; its per-decision
  work and redis memory grow with rate × window, where fixed_window and
  token_bucket are O(1) hashes. This run does not exercise that — it is
  a structural note, not a measured penalty (and the tails above can't
  substantiate one either way).
- **The burst run measures the textbook trade-off instead of asserting
  it.** One identical spike, one quota, three admitted counts —
  100 / 50 / 79 — that map exactly onto the three algorithms' contracts:
  fixed_window's boundary reset, sliding_window's exactness, token_bucket's
  burst-plus-refill. The counts are assertion-gated (fixed `> rate` and
  `≤ 2×rate`; sliding `== rate`; token inside a two-sided band around
  burst + duration×refill, so a broken refill fails the run), but the
  published figure is whatever was admitted.
- **Every replica saw an even third of the load and the histogram caught
  all of it.** Per-run, the three replicas' histogram `_count`s split
  evenly — 4000/4001/4000 summing to 12,001 (4002/4003/4002 → 12,007 for
  fixed_window/redis) sustained, 600/601/600 → 1801 burst (600/600/600 →
  1800 for sliding_window) — matching k6's `http_reqs` exactly. That equality is an asserted invariant of
  every run: no request went unmeasured or unaccounted, and no foreign
  traffic (metric scrapes, health checks) polluted the histogram.
