-- token_bucket: atomic check-and-consume for one key.
--   KEYS[1]  state hash {tokens, last_us}
--   ARGV[1]  burst (bucket capacity)
--   ARGV[2]  rate (tokens refilled per window)
--   ARGV[3]  window (microseconds, the refill interval)
-- Returns {allowed01, remaining, reset_us, retry_after_us}.
--
-- Time comes from the redis server (TIME); Redis 7 replicates scripts
-- by effects, making the write after the non-deterministic call legal.
local t = redis.call('TIME')
local now = t[1] * 1000000 + t[2]
local burst = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local refill = rate / window -- tokens per µs (Go: Rate/Window.Seconds() per second)

local st = redis.call('HMGET', KEYS[1], 'tokens', 'last_us')
local tokens, last
if st[1] then
  tokens, last = tonumber(st[1]), tonumber(st[2])
else
  -- Fresh key starts full, same as the memory backend's first step.
  tokens, last = burst, now
end
if now > last then
  tokens = math.min(burst, tokens + (now - last) * refill)
  last = now
end
-- now <= last: no elapsed time, or TIME stepped backward - grant no
-- free refill and leave last alone so a later, correctly-ordered call
-- still measures from the last known-good instant (parity with the
-- memory backend's guard).

-- Same epsilon as the memory backend's tokenEpsilon: float64 refill
-- sums can land one ULP under a mathematically-exact 1.0.
local allowed = tokens >= 1 - 1e-9
if allowed then
  tokens = tokens - 1
  if tokens < 0 then tokens = 0 end
end

-- Written on deny too - the refill above already mutated state (the
-- memory backend does the same). %.17g round-trips float64 exactly;
-- Lua's implicit tostring (%.14g) would corrupt both fields.
redis.call('HSET', KEYS[1],
  'tokens', string.format('%.17g', tokens),
  'last_us', string.format('%d', last))

-- TTL = time until the bucket refills to burst. A key expiring exactly
-- when full is lossless because a fresh key starts full - this is what
-- retires the memory backend's maxKeys cap on this backend. Post-
-- decision tokens < burst always (an allow just decremented; a deny
-- means tokens < 1 <= burst), so to_full > 0.
local to_full = (burst - tokens) / refill
local ttl = math.ceil(to_full / 1000)
if ttl < 1 then ttl = 1 end
redis.call('PEXPIRE', KEYS[1], ttl)

local remaining = math.floor(tokens)
local retry = 0
if not allowed then retry = math.ceil((1 - tokens) / refill) end
return {allowed and 1 or 0, remaining, now + math.ceil(to_full), retry}
