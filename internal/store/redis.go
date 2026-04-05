package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
)

// NewRedisClient creates a Redis connection from the URL.
// Returns nil if redisURL is empty (Redis is optional).
func NewRedisClient(redisURL string) *redis.Client {
	if redisURL == "" {
		slog.Warn("REDIS_URL not set — running without Redis buffer (webhook-direct mode)")
		return nil
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		slog.Error("invalid REDIS_URL", "error", err)
		return nil
	}

	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		slog.Error("redis ping failed — falling back to webhook-direct mode", "error", err)
		return nil
	}

	slog.Info(fmt.Sprintf("redis connected at %s", opts.Addr))
	return client
}
