package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/redis/go-redis/v9"
)

// One bounded hash per API key, with at most 32 short-lived receipts. Redis
// TIME avoids cross-instance clock drift. Neither raw receipts nor bodies are
// stored. Failed bindings cannot consume someone else's receipt.
const putWSFallbackReceiptScript = `
local now = tonumber(redis.call('TIME')[1])
local entries = redis.call('HGETALL', KEYS[1])
for i=1,#entries,2 do
  local expiry = tonumber(string.sub(entries[i+1], 1, 10))
  if not expiry or expiry <= now then redis.call('HDEL', KEYS[1], entries[i]) end
end
if redis.call('HLEN', KEYS[1]) >= 32 or redis.call('HEXISTS', KEYS[1], ARGV[1]) == 1 then return 0 end
redis.call('HSET', KEYS[1], ARGV[1], tostring(now + 120) .. ':' .. ARGV[2] .. ':' .. ARGV[3])
redis.call('EXPIRE', KEYS[1], 120)
return 1`

const consumeWSFallbackReceiptScript = `
local value = redis.call('HGET', KEYS[1], ARGV[1])
if not value then return '' end
local now = tonumber(redis.call('TIME')[1])
local expiry = tonumber(string.sub(value, 1, 10))
if not expiry or expiry <= now then
  redis.call('HDEL', KEYS[1], ARGV[1])
  return ''
end
if string.sub(value, 12, 75) ~= ARGV[2] then return '' end
redis.call('HDEL', KEYS[1], ARGV[1])
return string.sub(value, 77)`

var _ service.WSClientFallbackStore = (*gatewayCache)(nil)

func wsFallbackCacheKey(apiKeyID int64) string {
	return fmt.Sprintf("usage:ws_client_fallback:%d", apiKeyID)
}

func (c *gatewayCache) wsFallbackRedis() *redis.Client {
	// The primary Redis client may ignore context deadlines for socket I/O.
	// WithTimeout is a lightweight view sharing its pools, so bound this
	// optional observation without changing global settings or spawning work.
	// Never Close this view: the owning client closes the shared pools.
	return c.rdb.WithTimeout(100 * time.Millisecond)
}

func (c *gatewayCache) PutWSFallbackReceipt(ctx context.Context, apiKeyID int64, hash, binding, attemptID string) (bool, error) {
	if c == nil || c.rdb == nil || apiKeyID <= 0 || len(hash) != 64 || len(binding) != 64 || len(attemptID) != 36 {
		return false, nil
	}
	value, err := c.wsFallbackRedis().Eval(ctx, putWSFallbackReceiptScript, []string{wsFallbackCacheKey(apiKeyID)}, hash, binding, attemptID).Int()
	return value == 1, err
}

func (c *gatewayCache) ConsumeWSFallbackReceipt(ctx context.Context, apiKeyID int64, hash, binding string) (string, error) {
	if c == nil || c.rdb == nil || apiKeyID <= 0 || len(hash) != 64 || len(binding) != 64 {
		return "", nil
	}
	value, err := c.wsFallbackRedis().Eval(ctx, consumeWSFallbackReceiptScript, []string{wsFallbackCacheKey(apiKeyID)}, hash, binding).Text()
	if err == redis.Nil {
		return "", nil
	}
	return value, err
}
