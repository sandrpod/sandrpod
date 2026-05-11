//go:build !windows

// Copyright 2024 SandrPod
// os_version_unix.go — read OS distribution string for the WebSocket
// registration headers (X-Sandbox-OS-Version).

package main

import (
	"bufio"
	"os"
	"runtime"
	"strings"
)

// getOSVersion reads PRETTY_NAME from /etc/os-release on Linux. On macOS the
// file does not exist and we return "darwin" (the API server already gets the
// arch and goos via separate headers).
func getOSVersion() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			val := strings.TrimPrefix(line, "PRETTY_NAME=")
			val = strings.Trim(val, `"`)
			return val
		}
	}
	return runtime.GOOS
}
