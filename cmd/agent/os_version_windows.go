//go:build windows

// Copyright 2024 SandrPod
// os_version_windows.go — read Windows version string for the WebSocket
// registration headers (X-Sandbox-OS-Version).

package main

import (
	"os/exec"
	"runtime"
	"strings"
)

// getOSVersion shells out to `cmd /c ver` which prints e.g.
// "Microsoft Windows [Version 10.0.22631.4317]". The output is trimmed but
// otherwise preserved so the API server gets the full build number.
// Falls back to "windows" if cmd.exe is somehow unavailable.
func getOSVersion() string {
	out, err := exec.Command("cmd", "/c", "ver").Output()
	if err != nil {
		return runtime.GOOS
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return runtime.GOOS
	}
	return s
}
