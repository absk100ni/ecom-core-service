// Package cache provides a thin Redis-backed cache with graceful degradation.
//
// The cache is OPTIONAL: if Redis is unreachable or not configured, every operation
// becomes a safe no-op (Get reports a miss, Set silently does nothing). Callers must
// therefore always have a source-of-truth fallback — the cache only ever speeds up
// reads, it is never the system of record. This keeps a Redis outage from taking the
// API down with it.
//
// Usage:
//
//	c := cache.New(cfg.RedisAddr)        // nil-safe; logs once if unavailable
//	if v, ok := c.Get(ctx, key); ok { ... }
//	c.Set(ctx, key, payload, 6*time.Hour)
package cache

import (
	"context"
	"time"

	"ecom-core-service/pkg/logger"

	"github.com/redis/go-redis/v9"
)

var log = logger.New("CACHE", "REDIS")

// Cache wraps a redis client. A nil/disabled Cache is fully usable — all methods no-op.
type Cache struct {
	rdb     *redis.Client
	enabled bool
}

// New builds a Cache from a Redis address (e.g. "localhost:6379"). It pings once to
// decide whether caching is available; if not, the returned Cache degrades to no-ops.
// An empty addr disables caching entirely (e.g. when REDIS_ADDR is unset in dev).
func New(addr string) *Cache {
	if addr == "" {
		log.Info("New", "No REDIS_ADDR configured — cache disabled (pass-through)")
		return &Cache{enabled: false}
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		PoolSize:     10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Warn("New", "Redis unreachable — cache disabled (pass-through)", "addr", addr, "err", err.Error())
		return &Cache{rdb: rdb, enabled: false}
	}

	log.Info("New", "Redis cache connected", "addr", addr)
	return &Cache{rdb: rdb, enabled: true}
}

// Enabled reports whether the cache is live.
func (c *Cache) Enabled() bool { return c != nil && c.enabled }

// Get returns the cached string for key and whether it was a hit. Any error (including
// a missing key or a transient Redis failure) is reported as a miss — never an error —
// so callers transparently fall back to the source of truth.
func (c *Cache) Get(ctx context.Context, key string) (string, bool) {
	if !c.Enabled() {
		return "", false
	}
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		if err != redis.Nil {
			log.Debug("Get", "Redis get failed (treating as miss)", "key", key, "err", err.Error())
		}
		return "", false
	}
	return val, true
}

// Set stores value under key with a TTL. Failures are logged and swallowed — a cache
// write failing must never fail the request.
func (c *Cache) Set(ctx context.Context, key, value string, ttl time.Duration) {
	if !c.Enabled() {
		return
	}
	if err := c.rdb.Set(ctx, key, value, ttl).Err(); err != nil {
		log.Debug("Set", "Redis set failed (ignored)", "key", key, "err", err.Error())
	}
}

// RateAllow implements a fixed-window rate limiter shared across all app instances.
// It atomically increments a per-key counter and sets the window TTL on first hit.
// Returns (allowed, counted): counted is false when the cache is unavailable, signalling
// the caller to fall back to a local limiter rather than fail open globally.
func (c *Cache) RateAllow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, counted bool) {
	if !c.Enabled() {
		return true, false
	}
	n, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		log.Debug("RateAllow", "Redis incr failed (falling back to local)", "key", key, "err", err.Error())
		return true, false
	}
	if n == 1 {
		// First request in this window — start the expiry clock.
		c.rdb.Expire(ctx, key, window)
	}
	return n <= int64(limit), true
}

// Delete removes a key (best-effort).
func (c *Cache) Delete(ctx context.Context, key string) {
	if !c.Enabled() {
		return
	}
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		log.Debug("Delete", "Redis del failed (ignored)", "key", key, "err", err.Error())
	}
}

// DeleteByPrefix removes all keys matching "<prefix>*". It uses SCAN (cursor-based, non-
// blocking) rather than KEYS so it stays safe on large keyspaces. Best-effort.
func (c *Cache) DeleteByPrefix(ctx context.Context, prefix string) {
	if !c.Enabled() {
		return
	}
	var cursor uint64
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			log.Debug("DeleteByPrefix", "Redis scan failed (ignored)", "prefix", prefix, "err", err.Error())
			return
		}
		if len(keys) > 0 {
			c.rdb.Del(ctx, keys...)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

// Close releases the underlying connection pool.
func (c *Cache) Close() {
	if c != nil && c.rdb != nil {
		c.rdb.Close()
	}
}
