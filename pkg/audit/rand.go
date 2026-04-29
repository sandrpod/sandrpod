// Copyright 2026 SandrPod

package audit

import "crypto/rand"

// randByte returns one cryptographically-random byte. Wrapping a single
// crypto/rand.Read in a helper lets newEventID stay readable while still
// being non-predictable.
func randByte() byte {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail on Linux/macOS; if it does the
		// machine is in such a bad state that audit IDs are the least of
		// our problems. Fall back to a fixed byte rather than panic — the
		// resulting collisions are still detectable by EventID dedup.
		return 0
	}
	return b[0]
}
