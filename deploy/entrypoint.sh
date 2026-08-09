#!/bin/sh
# turnike live-demo entrypoint — one instance, any host that builds this
# Dockerfile (Render today, Fly.io equivalently). See DEPLOY.md.
#
# Three co-located processes in one machine: a socket-only redis and the echo
# upstream run in the background; the gateway runs in the foreground via
# `exec` so it becomes PID 1 and receives SIGTERM for a graceful drain.
#
# No in-container supervisor, by design (this is a demo, not an HA service):
#   - if redis dies, the gateway degrades to per-instance in-memory limiting
#     (on_error: degrade — real headers, still protected);
#   - if mock dies, the proxied routes answer 502;
#   - the platform restarts the instance only when the foreground gateway
#     exits, since that is the process it supervises.
set -eu

# Plain, ephemeral redis on a UNIX SOCKET with TCP switched off entirely
# (--port 0): no persistence (rate-limit keys are TTL'd and disposable), no
# auth (the socket is reachable only from inside this container, mode 0700
# under the same uid), and a writable working dir under /tmp for the non-root
# user. The memory cap is insurance for the 256 MB box: identity is
# unauthenticated client input, so a key-spray is bounded by eviction rather
# than an OOM (keys are TTL'd and disposable, so allkeys-lru eviction is safe).
#
# No TCP listener is the point, not a detail. A platform that probes its
# container's open ports to work out where to route traffic speaks HTTP at
# whatever it finds, and redis answers an HTTP verb on its wire protocol with
# "Possible SECURITY ATTACK detected" - correct behaviour, once a minute,
# forever, in the log of a demo whose readers will open that log. Removing
# the listener removes the probe's target, and drops the container's TCP
# surface to the one port that serves traffic.
redis-server \
	--unixsocket /tmp/redis.sock \
	--unixsocketperm 700 \
	--port 0 \
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
