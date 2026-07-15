-- sliding_window: atomic check-and-consume for one key.
--   KEYS[1]  zset log of accepted requests (score = accept time, µs)
--   ARGV[1]  rate (requests per window)
--   ARGV[2]  window (microseconds)
--   ARGV[3]  nonce - uniquifies members: TIME has µs resolution and
--            scripts run back-to-back, so two accepts in the same µs
--            are real; identical members would collapse into one zset
--            entry and silently over-admit.
-- Returns {allowed01, remaining, reset_us, retry_after_us}.
--
-- Like the memory backend, assumes TIME is non-decreasing per key. A
-- backward step never over-admits - future-scored entries still count
-- against rate - it only inflates Reset/RetryAfter by at most the step.
local t = redis.call('TIME')
local now = t[1] * 1000000 + t[2]
local rate = tonumber(ARGV[1])
local window = tonumber(ARGV[2])

-- Trim before EVERY decision. The inclusive max bound evicts an entry
-- exactly window old - parity with the memory backend's strict
-- ts.After(cutoff) keep. Trimming plus the TTL below is what bounds
-- this key: an unbounded zset would be M2's DoS lesson in redis form.
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - window)

local count = redis.call('ZCARD', KEYS[1])
local allowed = count < rate
if allowed then
  redis.call('ZADD', KEYS[1], now, string.format('%d-%s', now, ARGV[3]))
  count = count + 1
end
redis.call('PEXPIRE', KEYS[1], math.ceil(window / 1000))

local remaining = rate - count
if remaining < 0 then remaining = 0 end

-- Reset is when the log is completely clear again: the NEWEST counted
-- entry's expiry. RetryAfter is when the next single slot frees: the
-- OLDEST entry's. The two differ on purpose - under staggered load at
-- capacity one slot frees well before all of them do. (WITHSCORES
-- scores arrive as strings; tonumber is mandatory.)
local reset = now
local retry = 0
local newest = redis.call('ZRANGE', KEYS[1], -1, -1, 'WITHSCORES')
if newest[2] then reset = tonumber(newest[2]) + window end
if not allowed then
  local oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
  if oldest[2] then retry = tonumber(oldest[2]) + window - now end
end
return {allowed and 1 or 0, remaining, reset, retry}
