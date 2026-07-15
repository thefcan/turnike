# turnike

**turnike** — a turnstile for your APIs. A distributed rate limiter & API
gateway in Go, built in public. On the roadmap: hand-implemented token bucket,
sliding window and fixed window algorithms, atomic shared state via Redis+Lua,
and burst-load benchmarks against a multi-replica setup.

> 🚧 Work in progress. The full design-doc README (problem statement,
> architecture, measured trade-offs, multi-instance bypass demo) lands with
> milestone M6 — see [PROGRESS.md](PROGRESS.md) for current status.

## License

[MIT](LICENSE)
