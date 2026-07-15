// Package limiter will hold the rate-limiting algorithms (fixed window,
// token bucket, sliding window log) behind a common Limiter interface with
// an injected Clock for deterministic tests. Filled in by milestone M2;
// the Redis+Lua distributed backend follows in M3.
package limiter
