package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const grokBudgetKeyPrefix = "grok_budget:account:"

var reserveGrokBudgetScript = redis.NewScript(`
	local key = KEYS[1]
	local cost = tonumber(ARGV[1])
	local limit = tonumber(ARGV[2])
	local window = tonumber(ARGV[3])
	local current = tonumber(redis.call('GET', key) or '0')
	if current + cost > limit then
		return {0, current}
	end
	local used = redis.call('INCRBY', key, cost)
	local ttl = redis.call('TTL', key)
	if ttl < 0 then
		redis.call('EXPIRE', key, window)
	end
	return {1, used}
`)

type grokBudgetCache struct {
	rdb *redis.Client
}

func NewGrokBudgetCache(rdb *redis.Client) service.GrokBudgetCache {
	return &grokBudgetCache{rdb: rdb}
}

func (c *grokBudgetCache) GetGrokBudgetUsage(ctx context.Context, accountID int64, scope string, window time.Duration) (int, error) {
	if c == nil || c.rdb == nil {
		return 0, nil
	}
	key := grokBudgetKey(accountID, scope)
	value, err := c.rdb.Get(ctx, key).Int()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("grok budget get: %w", err)
	}
	return value, nil
}

func (c *grokBudgetCache) ReserveGrokBudget(ctx context.Context, accountID int64, scope string, cost, limit int, window time.Duration) (*service.GrokBudgetReservation, error) {
	if c == nil || c.rdb == nil {
		return &service.GrokBudgetReservation{Allowed: true, Scope: scope, Cost: cost, Limit: limit}, nil
	}
	if cost <= 0 || limit <= 0 || window <= 0 {
		return &service.GrokBudgetReservation{Allowed: true, Scope: scope, Cost: cost, Limit: limit}, nil
	}
	result, err := reserveGrokBudgetScript.Run(ctx, c.rdb, []string{grokBudgetKey(accountID, scope)}, cost, limit, int(window.Seconds())).Result()
	if err != nil {
		return nil, fmt.Errorf("grok budget reserve: %w", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) < 2 {
		return nil, fmt.Errorf("grok budget reserve: unexpected redis result %v", result)
	}
	allowed := redisInt(values[0]) == 1
	used := redisInt(values[1])
	return &service.GrokBudgetReservation{
		Allowed: allowed,
		Used:    used,
		Limit:   limit,
		Scope:   scope,
		Cost:    cost,
	}, nil
}

func grokBudgetKey(accountID int64, scope string) string {
	return fmt.Sprintf("%s%d:%s", grokBudgetKeyPrefix, accountID, scope)
}

func redisInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case string:
		var out int
		_, _ = fmt.Sscanf(v, "%d", &out)
		return out
	default:
		return 0
	}
}
