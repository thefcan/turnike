#!/bin/sh
# turnike live-demo entrypoint (Fly.io single instance, see DEPLOY.md).
#
# Three co-located processes in one machine: a plain localhost redis and the
# echo upstream run in the background; the gateway runs in the foreground via
# `exec` so it becomes PID 1 and receives SIGTERM for a graceful drain.
#
# No in-container supervisor, by design (this is a demo, not an HA service):
#   - if redis dies, the gateway degrades to per-instance in-memory limiting
#     (on_error: degrade — real headers, still protected);
#   - if mock dies, the proxied routes answer 502;
#   - Fly restarts the machine only when the foreground gateway exits.
set -eu

# Plain, ephemeral, localhost-only redis: no persistence (rate-limit keys are
# TTL'd and disposable), no auth (nothing off-box can reach 127.0.0.1), and a
# writable working dir under /tmp for the non-root user. The memory cap is
# insurance for the 256 MB box: identity is unauthenticated client input, so
# a key-spray is bounded by eviction rather than an OOM (keys are TTL'd and
# disposable, so allkeys-lru eviction is safe).
redis-server \
	--bind 127.0.0.1 \
	--port 6379 \
	--dir /tmp \
	--save '' \
	--appendonly no \
	--maxmemory 64mb \
	--maxmemory-policy allkeys-lru \
	--daemonize no &

# Echo upstream, bound to loopback so it is reachable only through the
# gateway, never from the internet.
mock -addr 127.0.0.1:9000 &

exec gateway -config /etc/turnike/config.yaml
