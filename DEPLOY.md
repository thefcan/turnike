# Deploying turnike

turnike deploys as a **single-instance demo**: one container, one public
service on `:8080`, with a **co-located plain redis** (`127.0.0.1:6379`, no
auth) and a **co-located echo upstream** (`127.0.0.1:9000`) started by
[`deploy/entrypoint.sh`](deploy/entrypoint.sh). It is a deliberate
simplification of the multi-box topology in the README — that one is what
`make demo` runs locally. No multi-region, no autoscaling, no custom domain.

**Verified locally:** the `deploy` image builds and runs on both `arm64` and
`amd64` — `/metrics` gated to 404, `200×5 → 429` with `X-RateLimit-*` and
`Retry-After`, decisions served by the real redis Lua path (`turnike:*` keys
present, not the degrade fallback), redis up as uid 65532 with a 64 MB cap.

**Host-agnostic by construction.** `deploy` is the Dockerfile's *last* stage,
so any host that builds a plain `Dockerfile` gets the right image without
knowing about build targets. A candidate host needs exactly three things:
Docker builds from the repo, a way to route traffic to port **8080** (the
config has no `PORT` support — `server.listen` is literal YAML), and pricing
you accept. Nothing else: no managed database (redis is in the container) and
no secrets (loopback redis needs no password).

Two targets are checked in — [`render.yaml`](render.yaml) (free, the default)
and [`fly.toml`](fly.toml) (paid, needs a card).

---

## Option A — Render (free, no payment method)

Render's free instance type needs no card, builds the repo's Dockerfile, and
gives 750 instance-hours a month at 512 MB — comfortably more than this
container needs. Without a payment method on file Render *cannot* bill you: if
a free limit is exceeded it suspends the service instead. The trade-off is
that a free service **sleeps after ~15 minutes of inactivity**, so the first
request after an idle spell waits ~30–60 s while it wakes.

**One-time setup** (all in the browser — no CLI):

1. Sign up at <https://render.com> with the GitHub account that owns the repo.
2. **New → Blueprint**, pick `thefcan/turnike`. Render reads
   [`render.yaml`](render.yaml) and proposes one free web service named
   `turnike`. Apply it.
   *(Fallback if Blueprints give trouble: **New → Web Service** → same repo →
   Language/Runtime **Docker** → Instance type **Free** → add an env var
   `PORT=8080` → Health check path `/healthz` → Create.)*
3. Wait for the first build. The public URL appears at the top of the service
   page, in the form `https://turnike.onrender.com` (Render appends a suffix
   if the name is taken — use whatever it shows).

**Redeploy:** push to `main` — `autoDeploy: true` rebuilds automatically.
Manual: **Manual Deploy → Deploy latest commit** on the service page.

**Logs:** the service page's *Logs* tab shows redis, mock and the gateway
booting, plus the per-request access log.

**Teardown:** service page → **Settings → Delete Service**.

## Option B — Fly.io (paid, needs a card)

Fly is pay-as-you-go: **a payment method is required** even though a
`shared-cpu-1x` / 256 MB machine with `auto_stop_machines` costs roughly a few
dollars a month or less. In exchange you get no forced sleep beyond the
scale-to-zero you configure, and a CLI-driven deploy.

```sh
brew install flyctl              # or: curl -L https://fly.io/install.sh | sh
fly auth login                   # opens a browser; needs a real terminal
fly apps create turnike          # names are global; if taken, pick another
                                 # and update `app` in fly.toml
fly config validate
make deploy                      # == DOCKER_DEFAULT_PLATFORM=linux/amd64 flyctl deploy --local-only
```

`make deploy` builds with your **local** Docker daemon, so the build context
never uploads to Fly's remote builder (`.dockerignore` excludes the local-only
files too). Fly Machines are amd64, hence the pinned platform — on Apple
Silicon that runs under emulation and is slower. Docker Desktop must be
running. Teardown: `fly apps destroy turnike`.

---

## Verify (over the public internet)

Replace the host with whatever your platform assigned.

```sh
HOST=https://turnike.onrender.com

# 1. A request goes through and carries the rate-limit budget headers.
#    On a sleeping free instance the FIRST call may take ~30-60s to wake it.
curl -i $HOST/demo/hello -H 'X-API-Key: try-me'
#    -> 200, with X-RateLimit-Limit / -Remaining / -Reset

# 2. Trip the limit — /demo is fixed_window 5-per-10s, so the 6th request
#    inside the window is a 429 with Retry-After:
for i in $(seq 1 8); do
  curl -s -o /dev/null -w '%{http_code} ' $HOST/demo/hello -H 'X-API-Key: try-me'
done; echo
#    -> 200 200 200 200 200 429 429 429

# 3. /metrics is NOT reachable from the internet (gated off this listener):
curl -s -o /dev/null -w '%{http_code}\n' $HOST/metrics
#    -> 404

# 4. Liveness:
curl -s $HOST/healthz    # -> ok
```

## Notes

- **Ephemeral redis.** The sidecar has no persistence; every restart comes up
  empty. Rate-limit keys are TTL'd and disposable, so this is harmless — a
  cold start just begins each window fresh.
- **No supervisor.** The entrypoint runs redis and mock in the background and
  the gateway in the foreground. If redis dies the gateway *degrades* to
  in-memory limiting (`on_error: degrade`, real headers); if mock dies the
  routes 502; the platform restarts the container only when the foreground
  gateway exits. Acceptable for a demo.
- **Observability stays local.** `/metrics` is gated off the public port
  (`server.metrics_disabled: true` in [`config.deploy.yaml`](config.deploy.yaml))
  and nothing scrapes the public instance. The Prometheus + Grafana degrade
  drill is the local `make demo` story in the README.
