// Package limiter implements the rate-limiting algorithms (fixed window,
// token bucket, sliding window log) behind a common Limiter interface,
// with an injected Clock for deterministic tests. The in-memory backend
// lives here; the Redis+Lua distributed backend follows in M3 behind the
// same interface, keyed turnike:{algo}:{key}.
package limiter
