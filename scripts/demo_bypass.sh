#!/usr/bin/env bash
# demo_bypass.sh — M4 multi-instance proof.
#
# Drives the SAME identity (X-API-Key) through nginx round-robining 3
# gateway replicas, twice: once with limiter.backend=memory (each
# replica keeps its own counters -> up to replicas x rate admitted:
# the bypass) and once with limiter.backend=redis (one shared counter
# -> exactly rate admitted). Raw per-request lines land in
# bench/demo_bypass_<backend>.txt; every published number traces back
# to those files.
#
# Assertions gate publication, but they check invariants only — the
# reported numbers are whatever was measured:
#   - zero unexpected statuses (a 503 means redis trouble under the
#     redis arm's fail_closed policy),
#   - exactly 3 distinct upstreams answered (a 2-replica topology bug
#     would admit 60 on memory and still look like "a bypass"),
#   - redis arm admitted == RATE (M3's proven atomicity property; holds
#     for ANY distribution of requests over replicas sharing one redis),
#   - memory arm admitted > RATE and <= 3*RATE (the bypass is visible
#     and physically bounded; exactly 3x needs an even split, which is
#     expected under strict round-robin but is an LB property, not a
#     limiter property).
#
# Compatible with macOS bash 3.2. Requires docker compose v2 and
# curl >= 7.83 (-w '%header{...}').
set -euo pipefail

cd "$(dirname "$0")/.."

readonly COMPOSE_FILE=docker-compose.demo.yml
readonly REQUESTS=150
readonly RATE=30
readonly REPLICAS=3
readonly LB=http://localhost:8090
readonly DEMO_PATH=/demo/hello
readonly API_KEY=demo-key

dc() { docker compose -f "$COMPOSE_FILE" "$@"; }

# die marks the current arm's raw file as unpublishable before exiting:
# a run that failed an assertion must not leave behind a complete-looking
# file that could be committed as a measurement.
CURRENT_OUT=""
die() {
    if [ -n "$CURRENT_OUT" ] && [ -f "$CURRENT_OUT" ]; then
        echo "# RUN INVALID: $*" >>"$CURRENT_OUT"
        mv "$CURRENT_OUT" "$CURRENT_OUT.failed"
        echo "demo_bypass: raw output marked invalid: $CURRENT_OUT.failed" >&2
    fi
    echo "demo_bypass: $*" >&2
    exit 1
}

# replica_addr prints the in-network address of gateway-<n> in the form
# nginx's $upstream_addr reports it.
replica_addr() {
    local cid
    cid=$(dc ps -q "gateway-$1")
    printf '%s:8080\n' "$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$cid")"
}

# --- preflight -------------------------------------------------------
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"
curl_ver=$(curl --version | awk 'NR == 1 { print $2 }')
awk -v v="$curl_ver" 'BEGIN {
    split(v, a, ".")
    exit !(a[1] > 7 || (a[1] == 7 && a[2] >= 83))
}' || die "curl >= 7.83 required for -w '%header{}' (found $curl_ver)"
[ -f demo/gateway-memory.yaml ] || die "demo/gateway-memory.yaml missing"
[ -f demo/gateway-redis.yaml ] || die "demo/gateway-redis.yaml missing"

trap 'dc down --volumes --remove-orphans >/dev/null 2>&1 || true' EXIT

mkdir -p bench

# Commit stamp captured once, before the run overwrites its own tracked
# outputs in bench/ - arm 1's regenerated file would otherwise mislabel
# arm 2 as -dirty.
COMMIT=$(git describe --always --dirty 2>/dev/null || echo unknown)
readonly COMMIT

# Start from nothing: `up -d --wait` happily reuses a leftover stack (a
# SIGKILLed run, a manual up) whose warm in-memory counters would
# publish a wrong memory-arm number that still passes the interval
# assertion below.
dc down --volumes --remove-orphans >/dev/null 2>&1 || true

echo "demo_bypass: building images..."
dc build

run_arm() {
    local backend=$1
    local out=bench/demo_bypass_${backend}.txt
    local i logs line now rem addr1 addr2 addr3

    echo "=== arm: backend=$backend ==="
    DEMO_BACKEND=$backend dc up -d --wait

    # Integrity guard: every replica must have booted with the intended
    # backend, so a mis-set DEMO_BACKEND can never mislabel an arm. The
    # gateway logs exactly one boot line with this key.
    for i in 1 2 3; do
        logs=$(dc logs "gateway-$i" 2>&1)
        echo "$logs" | grep -q "\"limiter_backend\":\"$backend\"" ||
            die "gateway-$i did not boot with backend=$backend"
    done

    # Warmup: --wait covers healthchecked services, but nginx has none
    # (a check routed through it would consume round-robin picks), so
    # poll until it accepts. /healthz is a reserved path and consumes
    # no quota; the RR offset these requests advance is irrelevant
    # because REQUESTS % REPLICAS == 0.
    for i in $(seq 1 40); do
        if curl -fsS -o /dev/null --max-time 2 "$LB/healthz" 2>/dev/null; then
            break
        fi
        [ "$i" -eq 40 ] && die "nginx did not become reachable at $LB"
        sleep 0.25
    done

    # Epoch-boundary guard: fixed_window's redis grid anchors at the
    # Unix epoch, so the 1h demo window rolls over exactly at the top
    # of the hour and a straddling arm would get a fresh quota mid-run.
    # An arm takes ~5-15s; 45s of margin is several times that, and if
    # it were ever exceeded the ==RATE / <=3*RATE assertions below
    # self-falsify (a boundary flake reads as "re-run", never as a
    # silently wrong number). The clock is read from redis itself: the
    # clock that actually decides windows (the Docker VM clock, shared
    # by every container, can drift from the host's on macOS).
    now=$(dc exec -T redis redis-cli TIME | head -n1 | tr -d '[:space:]')
    rem=$((3600 - now % 3600))
    if [ "$rem" -lt 45 ]; then
        echo "waiting ${rem}s for the top of the hour (fixed_window epoch grid)..."
        sleep $((rem + 1))
    fi

    # X-Demo-Upstream records only the replica's IP, and the IP ->
    # service mapping dies with the containers at down - so pin it in
    # the raw file header and resolve it in the footer while they live.
    addr1=$(replica_addr 1)
    addr2=$(replica_addr 2)
    addr3=$(replica_addr 3)

    {
        echo "# turnike demo_bypass - backend: $backend"
        echo "# date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "# commit: $COMMIT"
        echo "# topology: client -> nginx (round-robin, 1 worker) -> gateway-{1,2,3} -> mock; one shared redis"
        echo "# route: /demo/ fixed_window rate=$RATE window=1h; identity: X-API-Key: $API_KEY; requests=$REQUESTS, sequential"
        echo "# replica gateway-1: $addr1"
        echo "# replica gateway-2: $addr2"
        echo "# replica gateway-3: $addr3"
        echo "# columns: seq|status|x-ratelimit-remaining|upstream"
    } >"$out"
    CURRENT_OUT=$out

    echo "firing $REQUESTS requests..."
    for i in $(seq 1 "$REQUESTS"); do
        line=$(curl -sS -o /dev/null --max-time 10 \
            -H "X-API-Key: $API_KEY" \
            -w '%{http_code}|%header{x-ratelimit-remaining}|%header{x-demo-upstream}' \
            "$LB$DEMO_PATH") || die "request $i failed"
        printf '%s|%s\n' "$i" "$line" >>"$out"
    done

    # Negative proof for the memory arm: redis was up the whole time
    # (the topology is static), but no limiter key may exist in it -
    # the counting happened per replica, in memory.
    if [ "$backend" = memory ]; then
        keys=$(dc exec -T redis redis-cli --scan --pattern 'turnike:*' | tr -d '[:space:]')
        [ -z "$keys" ] || die "memory arm wrote redis keys: $keys"
        echo "# redis keys matching turnike:* after run: none" >>"$out"
    fi

    local total allowed denied other distinct maxrem
    total=$(awk -F'|' '!/^#/ { n++ } END { print n + 0 }' "$out")
    allowed=$(awk -F'|' '!/^#/ && $2 == 200 { n++ } END { print n + 0 }' "$out")
    denied=$(awk -F'|' '!/^#/ && $2 == 429 { n++ } END { print n + 0 }' "$out")
    other=$((total - allowed - denied))
    distinct=$(awk -F'|' '!/^#/ { u[$4] } END { n = 0; for (k in u) n++; print n }' "$out")
    maxrem=$(awk -F'|' '!/^#/ && $3 != "" { if ($3 + 0 > m) m = $3 + 0 } END { print m + 0 }' "$out")
    {
        echo "# summary: total=$total 200=$allowed 429=$denied other=$other"
        awk -F'|' -v a1="$addr1" -v a2="$addr2" -v a3="$addr3" '
            function name(u) {
                return u == a1 ? "gateway-1" : (u == a2 ? "gateway-2" : (u == a3 ? "gateway-3" : "unknown"))
            }
            !/^#/ { r[$4]++; if ($2 == 200) a[$4]++ }
            END { for (u in r) printf "# upstream %s (%s): requests=%d 200=%d\n", name(u), u, r[u], a[u] + 0 }' \
            "$out" | sort
    } >>"$out"

    dc down --volumes --remove-orphans

    [ "$total" -eq "$REQUESTS" ] || die "$backend arm: $total data lines, want $REQUESTS (see $out)"
    [ "$other" -eq 0 ] || die "$backend arm: $other unexpected statuses (see $out)"
    [ "$distinct" -eq "$REPLICAS" ] || die "$backend arm: $distinct distinct upstreams, want $REPLICAS (see $out)"
    # Freshness canary: the first request each counter ever sees must
    # report a full quota. Catches any warm-state path the
    # down-before-build belt misses.
    [ "$maxrem" -eq $((RATE - 1)) ] ||
        die "$backend arm: max remaining seen is $maxrem, want $((RATE - 1)) - counters were not fresh (see $out)"
    case $backend in
    redis)
        [ "$allowed" -eq "$RATE" ] ||
            die "redis arm admitted $allowed, want exactly $RATE (see $out)"
        ;;
    memory)
        [ "$allowed" -gt "$RATE" ] && [ "$allowed" -le $((REPLICAS * RATE)) ] ||
            die "memory arm admitted $allowed, want >$RATE and <=$((REPLICAS * RATE)) - bypass not visible (see $out)"
        ;;
    esac
    CURRENT_OUT=""

    echo "$backend: $allowed/$REQUESTS admitted, $denied denied, $distinct replicas -> $out"
}

run_arm memory
run_arm redis

echo
echo "=== comparison: one identity, limit $RATE per window, $REQUESTS requests through the LB ==="
for b in memory redis; do
    grep '^# summary' "bench/demo_bypass_$b.txt" | sed "s/^# summary:/  $b:/"
done
echo "raw outputs: bench/demo_bypass_memory.txt bench/demo_bypass_redis.txt"
