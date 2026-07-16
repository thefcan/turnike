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

## Latency / throughput

Not measured yet — the demo fires sequential single-connection requests,
which say nothing about latency under load. Burst-load k6 runs land with
M6; when they exist, produce them with a command recorded here and add
the percentile tables to this report.
