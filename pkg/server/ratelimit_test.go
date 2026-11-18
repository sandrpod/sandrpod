package server

import (
	"testing"
	"time"
)

func TestRateLimiterBurstAndRefill(t *testing.T) {
	rl := newRateLimiter(1) // burst = 2
	if !rl.allow("k") || !rl.allow("k") {
		t.Fatal("the first two calls should fit the burst")
	}
	if rl.allow("k") {
		t.Fatal("a third call in the same instant should be limited")
	}
	// Refill the single token a rate of 1/s owes after a second.
	rl.buckets["k"].last = time.Now().Add(-1100 * time.Millisecond)
	if !rl.allow("k") {
		t.Fatal("a token should have refilled")
	}
}

// TestRateLimiterSweepsStaleKeys guards against the map growing forever when
// the key is a client IP: an attacker rotating addresses must not turn the
// limiter into a memory leak.
func TestRateLimiterSweepsStaleKeys(t *testing.T) {
	rl := newRateLimiter(10)
	rl.buckets["old"] = &bucket{tokens: 1, last: time.Now().Add(-time.Hour)}
	rl.lastSweep = time.Now().Add(-2 * rateLimiterSweepEvery)

	rl.allow("new")
	if _, ok := rl.buckets["old"]; ok {
		t.Fatal("a bucket idle for an hour should have been swept")
	}
	if _, ok := rl.buckets["new"]; !ok {
		t.Fatal("the current key must survive the sweep")
	}
}
