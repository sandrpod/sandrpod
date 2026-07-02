// Copyright 2024 SandrPod
// Per-identity rate limiting for user-role tokens. Admin/system tokens are
// exempt (poders poll and heartbeat constantly). Disabled unless
// SANDRPOD_RATE_LIMIT (requests/second per identity) is set.

package main

import (
	"sync"
	"time"
)

// rateLimiter is a simple per-key token bucket (capacity = 2×rate, refill =
// rate/second). Good enough to stop accidental loops; not a DDoS control.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perSecond float64) *rateLimiter {
	return &rateLimiter{rate: perSecond, buckets: make(map[string]*bucket)}
}

// allow consumes one token for key, reporting whether the request may proceed.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.rate * 2, last: now}
		rl.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if max := rl.rate * 2; b.tokens > max {
		b.tokens = max
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
