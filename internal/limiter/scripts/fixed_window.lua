-- fixed_window: atomic check-and-consume for one key.
--   KEYS[1]  state hash {ws, count}
--   ARGV[1]  rate (requests per window)
--   ARGV[2]  window (microseconds)
-- Returns {allowed01, remaining, reset_us, retry_after_us}.
--
-- Time comes from the redis server (TIME), never a gateway clock, so
-- every instance decides on the same clock - that is the property this
-- backend exists for. Redis 7 replicates scripts by effects, which is
-- what makes writing after the non-deterministic TIME call legal.
--
-- The window grid anchors at the Unix epoch (now - now % window); the
-- in-memory backend anchors at Go's zero time via time.Truncate. Benign:
-- a deployment uses one backend, the two never share state, and all
-- gateway instances agree with each other because ws derives from TIME.
local t = redis.call('TIME')
local now = t[1] * 1000000 + t[2]
local rate = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local ws = now - (now % window)

local st = redis.call('HMGET', KEYS[1], 'ws', 'count')
local count = 0
if st[1] then
  local sws = tonumber(st[1])
  if sws >= ws then
    -- sws == ws: same window, adopt its count. sws > ws: TIME is
    -- gettimeofday, not monotonic - after a backward NTP step, keep the
    -- stored (newer) window instead of opening a fresh one, which would
    -- hand out up to rate extra admissions. The in-memory backend gets
    -- this for free from Go's monotonic clock reading.
    ws = sws
    count = tonumber(st[2])
  end
end

local allowed = count < rate
if allowed then
  count = count + 1
  -- string.format('%d'): Lua's implicit number-to-string uses %.14g,
  -- which would corrupt a µs epoch timestamp into scientific notation.
  redis.call('HSET', KEYS[1], 'ws', string.format('%d', ws), 'count', count)
  redis.call('PEXPIREAT', KEYS[1], math.ceil((ws + window) / 1000))
end
-- Deny writes nothing (parity: the memory backend does not count denied
-- requests). The key already carries a to-window-end TTL: count >= rate
-- >= 1 means an allowed write happened in this same window.

local remaining = rate - count
if remaining < 0 then remaining = 0 end
local reset = ws + window
local retry = 0
if not allowed then retry = reset - now end
return {allowed and 1 or 0, remaining, reset, retry}
