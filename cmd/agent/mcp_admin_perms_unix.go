//go:build !windows

package main

import (
	"log"
	"os"
)

// tightenSocketPerms restricts the AF_UNIX socket file to owner-only.
// On POSIX this is the auth boundary for /admin/* endpoints. Non-fatal
// because some filesystems (FUSE mounts, NFS, …) reject chmod.
func tightenSocketPerms(path string) {
	if err := os.Chmod(path, 0o600); err != nil {
		log.Printf("MCP admin: chmod %s failed: %v (continuing — may be world-accessible)", path, err)
	}
}
