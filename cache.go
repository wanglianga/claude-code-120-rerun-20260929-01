package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache 封装 Redis：溯源结果缓存 + 工单状态分布式锁（多生产线并行防并发改单）
type Cache struct {
	rdb *redis.Client
}

func newCache(rdb *redis.Client) *Cache { return &Cache{rdb: rdb} }

func traceKey(kind string, id int64) string {
	return fmt.Sprintf("trace:%s:%d", kind, id)
}

func (c *Cache) getTrace(ctx context.Context, kind string, id int64) (map[string]any, bool) {
	b, err := c.rdb.Get(ctx, traceKey(kind, id)).Bytes()
	if err != nil {
		return nil, false
	}
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		return nil, false
	}
	return v, true
}

func (c *Cache) setTrace(ctx context.Context, kind string, id int64, v any) {
	if b, err := json.Marshal(v); err == nil {
		_ = c.rdb.Set(ctx, traceKey(kind, id), b, 5*time.Minute).Err()
	}
}

func (c *Cache) invalidateTrace(ctx context.Context, rawIDs, fgIDs []int64) {
	for _, id := range rawIDs {
		_ = c.rdb.Del(ctx, traceKey("raw", id)).Err()
	}
	for _, id := range fgIDs {
		_ = c.rdb.Del(ctx, traceKey("fg", id)).Err()
	}
}

// withLock 以 SET NX PX 获取工单级分布式锁；返回释放函数
func (c *Cache) withLock(ctx context.Context, workOrderID int64, fn func() error) error {
	key := fmt.Sprintf("lock:wo:%d", workOrderID)
	token := fmt.Sprintf("%d-%d", workOrderID, time.Now().UnixNano())
	ok, err := c.rdb.SetNX(ctx, key, token, 30*time.Second).Result()
	if err != nil {
		// Redis 异常不阻断业务，但可能丢失串行保证
		return fn()
	}
	if !ok {
		return &apiError{code: 409,
			msg: fmt.Sprintf("工单 %d 正在被其他生产线/工序更新，请稍后重试", workOrderID)}
	}
	defer func() {
		// 仅持有者可释放（Lua 保证原子）
		_, _ = c.rdb.Eval(ctx,
			`if redis.call("get",KEYS[1])==ARGV[1] then return redis.call("del",KEYS[1]) else return 0 end`,
			[]string{key}, token).Result()
	}()
	return fn()
}

// withLockFallback Redis 不可用时的直通版本
func (s *Server) lockedByWorkOrder(ctx context.Context, workOrderID int64, fn func() error) error {
	if s.cache != nil {
		return s.cache.withLock(ctx, workOrderID, fn)
	}
	return fn()
}
