// Copyright 2026 SandrPod Contributors

package main

import "strings"

// sanitizeUTF8 strips invalid-UTF-8 byte sequences from s.
//
// Agent registration metadata (arch / OS / OS-version) arrives in HTTP headers
// controlled by the remote machine's locale and lands in the sandbox row. The
// SQL store targets Postgres, which enforces valid UTF-8 (SQLite did not), so a
// non-UTF-8 value makes the INSERT fail and the sandbox never persists — e.g. a
// Chinese Windows agent whose `cmd /c ver` emits GBK bytes (0xB0…) in
// X-Sandbox-OS-Version. Sanitizing at the DB boundary keeps the row insertable
// regardless of the agent's locale; the empty replacement drops the bad bytes
// rather than littering the value with U+FFFD.
func sanitizeUTF8(s string) string {
	return strings.ToValidUTF8(s, "")
}
