package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

func AuthMiddleware(apiKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if apiKey == "" {
			c.Next()
			return
		}

		key := c.GetHeader("X-API-Key")
		if key == "" {
			key = c.GetHeader("apikey")
		}
		if key == "" {
			auth := c.GetHeader("Authorization")
			key = strings.TrimPrefix(auth, "Bearer ")
		}

		if key != apiKey {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// RateLimiter implements a simple token bucket rate limiter per IP.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    int           // tokens per interval
	interval time.Duration
}

type bucket struct {
	tokens    int
	lastFill  time.Time
}

func NewRateLimiter(rate int, interval time.Duration) *RateLimiter {
	rl := &RateLimiter{
		buckets:  make(map[string]*bucket),
		rate:     rate,
		interval: interval,
	}
	// Cleanup stale buckets every 5 minutes
	go func() {
		for range time.Tick(5 * time.Minute) {
			rl.cleanup()
		}
	}()
	return rl
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	now := time.Now()

	if !ok {
		rl.buckets[key] = &bucket{tokens: rl.rate - 1, lastFill: now}
		return true
	}

	elapsed := now.Sub(b.lastFill)
	if elapsed >= rl.interval {
		refills := int(elapsed / rl.interval)
		b.tokens += refills * rl.rate
		if b.tokens > rl.rate {
			b.tokens = rl.rate
		}
		b.lastFill = now
	}

	if b.tokens > 0 {
		b.tokens--
		return true
	}
	return false
}

func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for key, b := range rl.buckets {
		if b.lastFill.Before(cutoff) {
			delete(rl.buckets, key)
		}
	}
}

// RateLimitMiddleware returns a Gin middleware that limits requests per IP.
func RateLimitMiddleware(rl *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if !rl.Allow(ip) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			c.Abort()
			return
		}
		c.Next()
	}
}
