package mcpbridge

import (
	"testing"
	"time"
)

func TestComputeBackoff(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 1 * time.Second},   // first restart
		{1, 2 * time.Second},   // 2nd consecutive
		{2, 4 * time.Second},   // 3rd
		{3, 8 * time.Second},   // 4th
		{4, 16 * time.Second},  // 5th
		{5, 30 * time.Second},  // 32s clamps to 30
		{6, 30 * time.Second},  // cap holds
		{50, 30 * time.Second}, // very high
		{-1, 1 * time.Second},  // negative -> base
	}
	for _, tc := range tests {
		got := computeBackoff(tc.failures)
		if got != tc.want {
			t.Errorf("computeBackoff(%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

func TestComputeBackoff_NeverExceedsRateLimitWindow(t *testing.T) {
	// Sanity: even the cap is < 60s, otherwise the rate-limiter (per-min)
	// becomes irrelevant — backoff alone would gate restarts.
	if restartBackoffMax >= 60*time.Second {
		t.Errorf("restartBackoffMax = %s; must be < 1 minute so the rate-limiter still kicks in for hopelessly-broken children", restartBackoffMax)
	}
}
