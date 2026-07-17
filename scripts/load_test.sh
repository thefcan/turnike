#!/usr/bin/env bash
# load_test.sh — M6 latency & admission measurements.
#
# Drives k6 (a pinned container joined to the demo network, so the
# Docker Desktop host<->VM port forward is never in the measured path)
# against the demo topology: nginx round-robin -> 3 gateway replicas ->
# mock, one shared redis. Two scenario shapes over scripts/k6/load.js:
#
#   sustained — 3 algorithms x {redis, memory}: constant arrival far
#               below the route quota, so nothing is denied and the
#               percentiles measure the allow path. The redis-vs-memory
#               delta per algorithm is the price of one shared-state
#               round trip.
#   burst     — 3 algorithms x redis, tight quota (rate 50 / 10s): an
#               identical ~6s spike timed to straddle a fixed_window
#               epoch-grid boundary, so the algorithms' admission
#               semantics are measured: fixed_window admits up to
#               2x rate across the boundary, sliding_window admits
#               exactly rate, token_bucket admits burst + refill.
#
# Each run gets a fresh stack (down --volumes / up --wait), so every
# replica's label-less request_duration histogram holds EXACTLY that
# run's observations: warmup polls /healthz and prometheus scrapes
# /metrics, both reserved paths the middleware never observes; k6
# traffic is the only routed traffic. Post-run (before down) the
# script scrapes each replica's /metrics directly and asserts
# k6 http_reqs == sum of the replicas' histogram _count.
#
# Assertions gate publication but check invariants only — the reported
# numbers are whatever was measured:
#   - every response is 200 or 429 (fail_closed turns mid-run redis
#     trouble into 503s, which invalidate the run loudly),
#   - dropped_iterations == 0 (the arrival schedule was actually kept),
#   - sustained: zero 429s (quota never interfered with latency),
#   - burst-fixed: rate < admitted <= 2*rate (the boundary hop is
#     visible and physically bounded),
#   - burst-sliding: admitted == rate exactly,
#   - burst-token: admitted within a two-sided band around
#     burst + duration*refill — a broken refill cannot pass,
#   - burst timing: k6's actual start within 0.5s of the redis-TIME-
#     derived target (the straddle really happened),
#   - k6 http_reqs == sum of replica histogram counts.
#
# Compatible with macOS bash 3.2. Requires docker compose v2, curl and
# jq (k6 summary parsing).
set -euo pipefail

cd "$(dirname "$0")/.."

readonly COMPOSE_FILE=docker-compose.demo.yml
readonly K6_IMAGE=grafana/k6:2.1.0
readonly NETWORK=turnike-demo_default
readonly BASE_URL=http://nginx:8080
readonly LB=http://localhost:8090
readonly API_KEY=load-key
readonly OUT_DIR=bench/load
readonly REPLICAS=3

# Load shape knobs. Overridable for smoke/calibration runs; the
# defaults are what bench/REPORT.md numbers are produced with.
readonly SUSTAINED_RATE=${SUSTAINED_RATE:-200}
readonly SUSTAINED_DURATION_S=${SUSTAINED_DURATION_S:-60}
readonly BURST_RATE=${BURST_RATE:-300}
readonly BURST_DURATION_S=6
readonly PRE_VUS=${PRE_VUS:-60}
readonly MAX_VUS=${MAX_VUS:-100}
# Optional comma-separated run-name filter, e.g.
# LOAD_ONLY=sustained_fixed_redis ./scripts/load_test.sh
readonly LOAD_ONLY=${LOAD_ONLY:-}

# Burst quota — must match the /burst-*/ routes in
# demo/gateway-load.yaml.
readonly BURST_QUOTA=50
readonly BURST_WINDOW_S=10
readonly BURST_LEAD_S=3  # burst starts this long before a boundary
readonly BURST_MIN_HEADROOM_S=5  # k6 create+init budget before start
# token_bucket admission band: burst + refill over the spike, two-sided
# so a broken refill (admitted == burst exactly) fails the run instead
# of publishing "burst absorption + refill: measured".
BURST_TOKEN_LOWER=$(awk -v q="$BURST_QUOTA" -v d="$BURST_DURATION_S" -v w="$BURST_WINDOW_S" \
    'BEGIN { printf "%d", q + 0.8 * d * (q / w) }')
BURST_TOKEN_UPPER=$(awk -v q="$BURST_QUOTA" -v d="$BURST_DURATION_S" -v w="$BURST_WINDOW_S" \
    'BEGIN { printf "%d", q + d * (q / w) + 3 }')
readonly BURST_TOKEN_LOWER BURST_TOKEN_UPPER

dc() { docker compose -f "$COMPOSE_FILE" "$@"; }

# die marks the current run's files unpublishable before exiting: a
# failed assertion must not leave complete-looking files that could be
# committed as measurements.
CURRENT_PREFIX=""
die() {
    if [ -n "$CURRENT_PREFIX" ]; then
        [ -f "$CURRENT_PREFIX.meta.txt" ] && echo "# RUN INVALID: $*" >>"$CURRENT_PREFIX.meta.txt"
        local f
        for f in "$CURRENT_PREFIX.meta.txt" "$CURRENT_PREFIX.summary.json" "$CURRENT_PREFIX.metrics.txt"; do
            [ -f "$f" ] && mv "$f" "$f.failed" &&
                echo "load_test: raw output marked invalid: $f.failed" >&2
        done
    fi
    echo "load_test: $*" >&2
    exit 1
}

# --- preflight -------------------------------------------------------
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"
command -v jq >/dev/null 2>&1 || die "jq is required (k6 summary parsing)"
command -v curl >/dev/null 2>&1 || die "curl is required"
[ -f demo/gateway-load.yaml ] || die "demo/gateway-load.yaml missing"
[ -f demo/gateway-load-memory.yaml ] || die "demo/gateway-load-memory.yaml missing"
[ -f scripts/k6/load.js ] || die "scripts/k6/load.js missing"

trap 'dc down --volumes --remove-orphans >/dev/null 2>&1 || true' EXIT

mkdir -p "$OUT_DIR"

# Commit stamp captured once, before any run regenerates tracked files
# in bench/ (the demo_bypass lesson: run 1 would mislabel run 2 as
# -dirty).
COMMIT=$(git describe --always --dirty 2>/dev/null || echo unknown)
readonly COMMIT

# Pin the load generator: pre-pull so image download can never eat the
# burst-timing budget, and record exactly what ran.
echo "load_test: pulling $K6_IMAGE..."
docker pull "$K6_IMAGE" >/dev/null || die "cannot pull $K6_IMAGE"
K6_DIGEST=$(docker inspect --format '{{index .RepoDigests 0}}' "$K6_IMAGE" 2>/dev/null || echo unknown)
K6_VERSION=$(docker run --rm "$K6_IMAGE" version 2>/dev/null | head -n1 || echo unknown)
RIG_NCPU=$(docker info --format '{{.NCPU}}' 2>/dev/null || echo unknown)
RIG_MEM=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo unknown)
RIG_MACOS=$(sw_vers -productVersion 2>/dev/null || echo unknown)
readonly K6_DIGEST K6_VERSION RIG_NCPU RIG_MEM RIG_MACOS

# Start from nothing: a leftover stack would carry warm histograms.
dc down --volumes --remove-orphans >/dev/null 2>&1 || true

echo "load_test: building images..."
dc build >/dev/null

# expected_backend <arm> — the boot-log integrity value per config arm.
expected_backend() {
    case $1 in
    load) echo redis ;;
    load-memory) echo memory ;;
    *) die "unknown arm: $1" ;;
    esac
}

# jq_count <file> <metric> — counter value, 0 if the metric was never
# emitted (k6 omits empty metrics from the summary).
jq_count() {
    jq -r ".metrics[\"$2\"].values.count // 0" "$1"
}

# hist_quantiles <metrics-file> — sums the replicas' cumulative buckets
# and prints, per quantile, the bounding bucket and the
# histogram_quantile-style linear interpolation:
#   q=0.50 lo=0.001 hi=0.0025 interp=0.001640 total=12000
# REPORT.md leads with the (lo, hi] bound; the interpolation is the
# parenthetical (uniform-within-bucket is an assumption, not a
# measurement).
hist_quantiles() {
    awk '
        /^turnike_request_duration_seconds_bucket\{le="/ {
            le = $0
            sub(/^[^"]*"/, "", le)
            sub(/".*/, "", le)
            if (le == "+Inf") le = "inf"
            sum[le] += $2
            if (!(le in seen)) { seen[le] = 1; edges[++n] = le }
        }
        /^turnike_request_duration_seconds_count[ ]/ { total += $2 }
        END {
            # numeric sort of the edges (BSD awk: no asort)
            for (i = 1; i <= n; i++)
                for (j = i + 1; j <= n; j++) {
                    a = edges[i] == "inf" ? 1e308 : edges[i] + 0
                    b = edges[j] == "inf" ? 1e308 : edges[j] + 0
                    if (b < a) { t = edges[i]; edges[i] = edges[j]; edges[j] = t }
                }
            if (total == 0 || sum[edges[n]] != total)
                { printf "ERROR bucket/count mismatch: inf=%d total=%d\n", sum[edges[n]], total; exit 1 }
            split("0.50 0.95 0.99", qs, " ")
            for (k = 1; k <= 3; k++) {
                q = qs[k]; rank = q * total; prev = 0; lo = 0
                for (i = 1; i <= n; i++) {
                    le = edges[i]; cum = sum[le]
                    if (cum >= rank) {
                        if (le == "inf")
                            { printf "q=%s lo=%s hi=inf interp=inf total=%d\n", q, lo, total }
                        else {
                            hi = le + 0
                            interp = lo + (hi - lo) * (rank - prev) / (cum - prev)
                            printf "q=%s lo=%s hi=%s interp=%.6f total=%d\n", q, lo, le, interp, total
                        }
                        break
                    }
                    prev = cum
                    lo = le == "inf" ? lo : le
                }
            }
        }
    ' "$1"
}

# decisions <metrics-file> <route> — requests_total by decision for the
# exercised route, summed across replicas.
decisions() {
    awk -v route="$2" '
        /^turnike_requests_total\{/ {
            if (match($0, /decision="[^"]*"/)) d = substr($0, RSTART + 10, RLENGTH - 11)
            if (match($0, /route="[^"]*"/)) r = substr($0, RSTART + 7, RLENGTH - 8)
            if (r == route) sum[d] += $2
        }
        END {
            printf "allow=%d deny=%d degrade_allow=%d degrade_deny=%d degrade=%d\n", \
                sum["allow"], sum["deny"], sum["degrade_allow"], sum["degrade_deny"], sum["degrade"]
        }
    ' "$1"
}

# run_load <name> <arm> <route> <scenario> <rate> <duration_s>
run_load() {
    local name=$1 arm=$2 route=$3 scenario=$4 rate=$5 duration_s=$6
    local prefix=$OUT_DIR/$name
    local backend algo start_epoch now i logs k6log rem

    if [ -n "$LOAD_ONLY" ]; then
        case ",$LOAD_ONLY," in
        *",$name,"*) ;;
        *) echo "skip: $name (LOAD_ONLY)"; return 0 ;;
        esac
    fi

    backend=$(expected_backend "$arm")
    case $name in
    *_fixed_*) algo=fixed_window ;;
    *_token_*) algo=token_bucket ;;
    *_sliding_*) algo=sliding_window ;;
    esac

    echo "=== run: $name (arm=$arm route=$route $scenario rate=${rate}/s ${duration_s}s) ==="
    dc down --volumes --remove-orphans >/dev/null 2>&1 || true
    DEMO_BACKEND=$arm dc up -d --wait

    # Integrity guard: every replica must have booted with the intended
    # backend — a mis-set DEMO_BACKEND cannot mislabel an arm.
    for i in 1 2 3; do
        logs=$(dc logs "gateway-$i" 2>&1)
        echo "$logs" | grep -q "\"limiter_backend\":\"$backend\"" ||
            die "gateway-$i did not boot with backend=$backend"
    done

    # Warmup OUTSIDE k6 (a k6-driven warmup would inflate http_reqs
    # with requests the histogram never... would in fact see — either
    # way the counts would no longer be the scenario's). /healthz is
    # reserved: consumes no quota, observes no histogram sample.
    for i in $(seq 1 40); do
        if curl -fsS -o /dev/null --max-time 2 "$LB/healthz" 2>/dev/null; then
            break
        fi
        [ "$i" -eq 40 ] && die "nginx did not become reachable at $LB"
        sleep 0.25
    done

    # Burst timing: derive the start instant from redis TIME — the
    # clock that decides windows (the Docker VM clock can drift from
    # the host's on macOS). Aim BURST_LEAD_S before the next 10s
    # epoch-grid boundary that leaves enough headroom for k6
    # create+init; load.js busy-waits to the instant in-process, so
    # container start latency spends none of the straddle budget.
    start_epoch=0
    if [ "$scenario" = burst ]; then
        now=$(dc exec -T redis redis-cli TIME | head -n1 | tr -d '[:space:]')
        start_epoch=$((now - now % BURST_WINDOW_S + BURST_WINDOW_S - BURST_LEAD_S))
        while [ $((start_epoch - now)) -lt "$BURST_MIN_HEADROOM_S" ]; do
            start_epoch=$((start_epoch + BURST_WINDOW_S))
        done
    fi

    local policy_note=""
    [ "$backend" = redis ] && policy_note=", on_error=fail_closed"
    {
        echo "# turnike load_test - run: $name"
        echo "# date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "# commit: $COMMIT"
        echo "# rig: docker NCPU=$RIG_NCPU mem_bytes=$RIG_MEM macos=$RIG_MACOS (laptop; numbers are for relative comparison on this rig)"
        echo "# load generator: $K6_IMAGE ($K6_DIGEST; $K6_VERSION), in-network (docker run --network $NETWORK) -> $BASE_URL"
        echo "# topology: k6 -> nginx (round-robin, 1 worker, no upstream keepalive) -> gateway-{1,2,3} -> mock; one shared redis"
        echo "# arm: DEMO_BACKEND=$arm (limiter backend=$backend$policy_note)"
        echo "# route: $route $algo (quota in demo/gateway-$arm.yaml at the stamped commit); identity: X-API-Key: $API_KEY (single key -> single quota)"
        echo "# scenario: $scenario arrival=${rate}/s duration=${duration_s}s preVUs=$PRE_VUS maxVUs=$MAX_VUS start_epoch_s=$start_epoch"
        echo "# files: $name.summary.json (k6 end-of-test summary), $name.metrics.txt (per-replica /metrics scrape)"
    } >"$prefix.meta.txt"
    CURRENT_PREFIX=$prefix

    k6log=$(mktemp)
    echo "firing k6 ($scenario)..."
    docker run --rm -i \
        --network "$NETWORK" \
        -v "$PWD/scripts/k6:/scripts:ro" \
        -v "$PWD/$OUT_DIR:/out" \
        -e BASE_URL="$BASE_URL" -e ROUTE="$route" -e SCENARIO="$scenario" \
        -e RATE="$rate" -e DURATION="${duration_s}s" -e START_EPOCH_S="$start_epoch" \
        -e NAME="$name" -e API_KEY="$API_KEY" -e PRE_VUS="$PRE_VUS" -e MAX_VUS="$MAX_VUS" \
        "$K6_IMAGE" run --quiet /scripts/load.js >"$k6log" 2>&1 ||
        { cat "$k6log" >&2; die "$name: k6 run failed"; }

    # Per-replica scrape BEFORE down: the state dies with the stack.
    # One scrape per replica; the per-replica count is read from the
    # same bytes that land in the raw file.
    : >"$prefix.metrics.txt"
    local count_split="" scrape cnt
    for i in 1 2 3; do
        scrape=$(dc exec -T "gateway-$i" wget -qO- http://127.0.0.1:8080/metrics) ||
            die "$name: metrics scrape of gateway-$i failed"
        {
            echo "# === gateway-$i /metrics ==="
            echo "$scrape"
        } >>"$prefix.metrics.txt"
        cnt=$(echo "$scrape" | awk '/^turnike_request_duration_seconds_count[ ]/ { c = $2 } END { print c + 0 }')
        count_split="$count_split gateway-$i=$cnt"
    done

    local summary=$prefix.summary.json
    [ -s "$summary" ] || die "$name: k6 summary export missing"

    # --- measured numbers, straight from the raw files ---------------
    local reqs dropped s200 s429 sother durline offset_line actual_ms offset_ok
    reqs=$(jq_count "$summary" http_reqs)
    dropped=$(jq_count "$summary" dropped_iterations)
    s200=$(jq_count "$summary" status_200)
    s429=$(jq_count "$summary" status_429)
    sother=$(jq_count "$summary" status_other)
    durline=$(jq -r '.metrics.http_req_duration.values | "avg=\(.avg) min=\(.min) med=\(.med) p90=\(.["p(90)"]) p95=\(.["p(95)"]) p99=\(.["p(99)"]) max=\(.max)"' "$summary")

    local hist total_hist
    hist=$(hist_quantiles "$prefix.metrics.txt") || die "$name: histogram parse failed: $hist"
    case $hist in *ERROR*) die "$name: $hist" ;; esac
    total_hist=$(echo "$hist" | head -n1 | sed 's/.*total=//')

    local dec
    dec=$(decisions "$prefix.metrics.txt" "$route")

    {
        echo "measured: http_reqs=$reqs status_200=$s200 status_429=$s429 other=$sother dropped_iterations=$dropped"
        echo "k6 http_req_duration ms: $durline"
        echo "gateway histogram (replicas summed): count=$total_hist"
        echo "$hist" | sed 's/^/gateway /'
        echo "per-replica histogram _count:$count_split"
        echo "decisions $route: $dec"
    } >>"$prefix.meta.txt"

    # Burst-timing evidence: the k6 log line records the target (from
    # redis TIME) and the actual in-process start (VM clock — the same
    # kernel clock redis reads).
    if [ "$scenario" = burst ]; then
        offset_line=$(grep -o 'scenario_start[^"]*' "$k6log" | head -n1)
        [ -n "$offset_line" ] || die "$name: scenario_start line missing from k6 log"
        echo "# $offset_line" >>"$prefix.meta.txt"
        actual_ms=$(echo "$offset_line" | sed -E 's/.*actual_start_ms=([0-9]+).*/\1/')
        offset_ok=$(awk -v a="$actual_ms" -v t="$start_epoch" \
            'BEGIN { d = a / 1000 - t; if (d < 0) d = -d; if (d <= 0.5) print "ok"; else print "off by " d "s" }') ||
            die "$name: offset check failed to evaluate"
        [ "$offset_ok" = ok ] || die "$name: burst start missed target: $offset_ok"
    fi
    rm -f "$k6log"

    # --- invariants gate publication ----------------------------------
    [ "$dropped" -eq 0 ] || die "$name: $dropped dropped iterations - arrival schedule not kept"
    [ "$sother" -eq 0 ] || die "$name: $sother unexpected statuses (redis trouble under fail_closed?)"
    [ $((s200 + s429)) -eq "$reqs" ] || die "$name: status split $s200+$s429 != $reqs"
    [ "$reqs" -eq "$total_hist" ] ||
        die "$name: k6 sent $reqs but replicas observed $total_hist - histogram integrity broken"
    echo "$dec" | grep -q 'degrade_allow=0 degrade_deny=0 degrade=0$' ||
        die "$name: degrade decisions recorded mid-run: $dec"
    # Cross-check the two views of the same run: every 200 is the
    # backend's own allow verdict, every 429 its deny (mock never
    # fails, and a fail_closed 503 is status_other, asserted 0 above).
    local dec_allow dec_deny
    dec_allow=$(echo "$dec" | sed -E 's/^allow=([0-9]+) .*/\1/')
    dec_deny=$(echo "$dec" | sed -E 's/^allow=[0-9]+ deny=([0-9]+) .*/\1/')
    [ "$dec_allow" -eq "$s200" ] && [ "$dec_deny" -eq "$s429" ] ||
        die "$name: decision counts (allow=$dec_allow deny=$dec_deny) disagree with statuses (200=$s200 429=$s429)"

    case $scenario in
    sustained)
        [ "$s429" -eq 0 ] || die "$name: $s429 denials in a sustained run - quota interfered with latency"
        ;;
    burst)
        case $algo in
        fixed_window)
            [ "$s200" -gt "$BURST_QUOTA" ] && [ "$s200" -le $((2 * BURST_QUOTA)) ] ||
                die "$name: admitted $s200, want >$BURST_QUOTA and <=$((2 * BURST_QUOTA)) (boundary hop not visible)"
            ;;
        sliding_window)
            [ "$s200" -eq "$BURST_QUOTA" ] ||
                die "$name: admitted $s200, want exactly $BURST_QUOTA (sliding exactness broken)"
            ;;
        token_bucket)
            [ "$s200" -ge "$BURST_TOKEN_LOWER" ] && [ "$s200" -le "$BURST_TOKEN_UPPER" ] ||
                die "$name: admitted $s200, want $BURST_TOKEN_LOWER..$BURST_TOKEN_UPPER (burst+refill band)"
            ;;
        esac
        ;;
    esac

    echo "# assertions: PASS" >>"$prefix.meta.txt"
    CURRENT_PREFIX=""

    dc down --volumes --remove-orphans >/dev/null

    echo "$name: reqs=$reqs 200=$s200 429=$s429 -> $prefix.meta.txt"
}

# --- the recorded matrix ---------------------------------------------
run_load sustained_fixed_redis load /fixed/ sustained "$SUSTAINED_RATE" "$SUSTAINED_DURATION_S"
run_load sustained_token_redis load /token/ sustained "$SUSTAINED_RATE" "$SUSTAINED_DURATION_S"
run_load sustained_sliding_redis load /sliding/ sustained "$SUSTAINED_RATE" "$SUSTAINED_DURATION_S"
run_load sustained_fixed_memory load-memory /fixed/ sustained "$SUSTAINED_RATE" "$SUSTAINED_DURATION_S"
run_load sustained_token_memory load-memory /token/ sustained "$SUSTAINED_RATE" "$SUSTAINED_DURATION_S"
run_load sustained_sliding_memory load-memory /sliding/ sustained "$SUSTAINED_RATE" "$SUSTAINED_DURATION_S"
run_load burst_fixed_redis load /burst-fixed/ burst "$BURST_RATE" "$BURST_DURATION_S"
run_load burst_token_redis load /burst-token/ burst "$BURST_RATE" "$BURST_DURATION_S"
run_load burst_sliding_redis load /burst-sliding/ burst "$BURST_RATE" "$BURST_DURATION_S"

echo
echo "=== load_test complete; raw outputs in $OUT_DIR/ ==="
for f in "$OUT_DIR"/*.meta.txt; do
    [ -f "$f" ] || continue
    echo "--- $f"
    grep '^measured:' "$f" || true
done
