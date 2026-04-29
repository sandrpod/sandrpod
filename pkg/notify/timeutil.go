// Copyright 2026 SandrPod

package notify

import "time"

// timeUntil returns the seconds remaining until t (>= 0).
func timeUntil(t time.Time) float64 {
	d := time.Until(t)
	if d < 0 {
		return 0
	}
	return d.Seconds()
}
