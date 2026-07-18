# Deploying turnike (Fly.io)

turnike runs as a **single-instance live demo** on Fly.io: one machine, one
public service on `:8080`, with a **co-located plain redis** (`127.0.0.1:6379`,
no auth) and a **co-located echo upstream** (`127.0.0.1:9000`) started by
[`deploy/entrypoint.sh`](deploy/entrypoint.sh). It is a deliberate
simplification of the multi-box topology in the README — that one is what
`make demo` runs locally.

This is a demo deployment: no multi-region, no autoscaling, no custom domain.

## ⚠️ Before you start — account & cost

- **A Fly.io account with a payment method (card) is required.** Fly is
  pay-as-you-go, not a free tier.
- **Cost:** one `shared-cpu-1x` / 256 MB machine. With `auto_stop_machines`
  (this repo's default) it scales to zero when idle, so the bill is roughly a
  few dollars a month or less. Set `min_machines_running = 1` in `fly.toml`
  for an always-warm box (snappier, a bit more cost).
- **No secrets, no managed database to provision.** redis is a loopback
  sidecar with no password, so there is nothing to put in `fly secrets`.

## One-time setup

```sh
# 1. Install flyctl (macOS)
brew install flyctl            # or: curl -L https://fly.io/install.sh | sh

# 2. Log in (opens a browser). Run it yourself:
fly auth login

# 3. Create the app (does not deploy). App names are globally unique; if
#    "turnike" is taken, use another and update `app` in fly.toml + the
#    README "Try it live" URL to match. The region comes from fly.toml
#    (primary_region = "fra"); change it there if you prefer another.
fly apps create turnike

# 4. Sanity-check the config schema before the first deploy.
fly config validate
```

## Deploy

```sh
make deploy      # == flyctl deploy --local-only
```

`make deploy` builds the image with your **local** Docker daemon
(`--local-only`), so the build context never uploads to Fly's remote builder —
the local-only agent files never leave this machine (`.dockerignore` excludes
them too). Fly Machines are amd64, so the build is pinned to `linux/amd64`
(`DOCKER_DEFAULT_PLATFORM`); on Apple Silicon that runs under emulation and is
a bit slower. Docker Desktop must be running.

## Verify (over the public internet)

Replace `turnike.fly.dev` with your app's hostname if you changed the name.

```sh
# 1. A request goes through and carries the rate-limit budget headers:
curl -i https://turnike.fly.dev/demo/hello -H 'X-API-Key: try-me'
#    -> HTTP/2 200, with X-RateLimit-Limit / -Remaining / -Reset

# 2. Trip the limit — the 6th request in 10s is a 429 with Retry-After:
for i in $(seq 1 8); do
  curl -s -o /dev/null -w '%{http_code} ' https://turnike.fly.dev/demo/hello -H 'X-API-Key: try-me'
done; echo
#    -> 200 200 200 200 200 429 429 429

# 3. /metrics is NOT reachable from the internet (gated off this listener):
curl -s -o /dev/null -w '%{http_code}\n' https://turnike.fly.dev/metrics
#    -> 404

# 4. Liveness:
curl -s https://turnike.fly.dev/healthz    # -> ok
```

`fly logs` shows the three processes booting (redis, mock, gateway) and the
per-request access log.

## Redeploy

```sh
make deploy      # idempotent; ships the current working tree
```

## Teardown

```sh
fly apps destroy turnike
```

## Notes

- **Ephemeral redis.** The sidecar has no persistence; on every machine
  start it comes up empty. Rate-limit keys are TTL'd and disposable, so this
  is harmless — a cold start just begins each window fresh.
- **No supervisor.** The entrypoint runs redis and mock in the background and
  the gateway in the foreground. If redis dies the gateway *degrades* to
  in-memory limiting (`on_error: degrade`, real headers); if mock dies the
  routes 502; Fly restarts the machine only when the foreground gateway
  exits. Acceptable for a demo.
- **Observability stays local.** `/metrics` is gated off the public port and
  nothing scrapes prod. The Prometheus + Grafana degrade drill is the local
  `make demo` / `docker compose` story in the README, not a public endpoint.
