// Copyright 2026 SandrPod Contributors
// Per-Identity rate limiting for user-role tokens. Admin/system tokens are
// exempt (poders poll and heartbeat constantly). Disabled unless
// SANDRPOD_RATE_LIMIT (requests/second per Identity) is set.

package server

import (
	"sync"
	"time"
)

// rateLimiter is a simple per-key token bucket (capacity = 2×rate, refill =
// rate/second). Good enough to stop accidental loops; not a DDoS control.
//
// Keys can be caller-controlled (a client IP), so entries are swept: without
// that, rotating addresses grows the map without bound and the limiter becomes
// the memory-exhaustion path it was meant to prevent.
type rateLimiter struct {
	mu        sync.Mutex
	rate      float64
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	rateLimiterSweepEvery = time.Minute
	rateLimiterStaleAfter = 10 * time.Minute
)

func newRateLimiter(perSecond float64) *rateLimiter {
	return &rateLimiter{rate: perSecond, buckets: make(map[string]*bucket)}
}

// allow consumes one token for key, reporting whether the request may proceed.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	if now.Sub(rl.lastSweep) > rateLimiterSweepEvery {
		for k, b := range rl.buckets {
			if now.Sub(b.last) > rateLimiterStaleAfter {
				delete(rl.buckets, k)
			}
		}
		rl.lastSweep = now
	}
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
