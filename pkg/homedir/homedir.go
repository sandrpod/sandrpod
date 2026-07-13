// Copyright 2026 SandrPod
// Package homedir resolves the base directory for sandrpod's own on-disk data
// — the "~/.sandrpod" tree: permissions.json, mcp.json, mcp_grants.json, OAuth
// tokens, audit logs, the personal skills dir, and the authz/mcp unix sockets.
//
// On a normal login it is exactly os.UserHomeDir(). The one exception is a
// Windows service running under LocalSystem: there os.UserHomeDir() (and
// %USERPROFILE% / %APPDATA%) resolve under
// C:\Windows\System32\config\systemprofile — INSIDE the protected System32
// tree. sandrpod's data would then live there, and the toolbox file gate's
// Windows blacklist (which correctly guards System32\config, home of the
// SAM/registry hives) blocks reads of it — e.g. the personal skills dir 403s.
// For that account we redirect to %ProgramData% (C:\ProgramData), the Windows
// convention for machine/service-scoped application data: writable by the
// service account and outside System32.
//
// This relocates only sandrpod's OWN data. User-path references — tilde
// expansion of permission rules like "~/.ssh", and the account's reported home
// in env info — deliberately keep os.UserHomeDir() so hardlocks and reports
// reflect the actual account the agent runs as.
//
// NOTE: running the agent as LocalSystem is a degenerate mode. The employee-PC
// design expects the agent to run in the user's session — only then can it
// show consent dialogs and share the ~/.sandrpod tree (incl. authz.sock) with
// the tray. This redirect keeps a service-account agent self-consistent and
// unblocked; it is not a substitute for running in the user session.
package homedir

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DataHome returns the base directory under which the ".sandrpod" data tree
// lives (DataDir == DataHome()/.sandrpod). It is os.UserHomeDir() except on a
// Windows service account, where it is %ProgramData%.
func DataHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		switch {
		case os.Getenv("HOME") != "":
			home = os.Getenv("HOME")
		case os.Getenv("USERPROFILE") != "":
			home = os.Getenv("USERPROFILE")
		default:
			home = os.TempDir()
		}
	}
	if runtime.GOOS == "windows" && IsWindowsServiceProfile(home) {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return pd
		}
		return os.TempDir()
	}
	return home
}

// DataDir returns DataHome()/.sandrpod, the base for all sandrpod data.
func DataDir() string { return filepath.Join(DataHome(), ".sandrpod") }

// IsWindowsServiceProfile reports whether p is the Windows LocalSystem/service
// account profile (…\System32\config\systemprofile, or the 32-bit SysWOW64
// variant). Exported so callers that build paths from other env vars
// (%APPDATA%) can apply the same redirect.
func IsWindowsServiceProfile(p string) bool {
	// Normalize backslashes explicitly (not filepath.ToSlash, which is a no-op
	// on '\' when this runs on a non-Windows host) so the check is correct
	// regardless of the OS evaluating it.
	s := strings.ReplaceAll(strings.ToLower(p), `\`, "/")
	return strings.Contains(s, "/system32/config/systemprofile") ||
		strings.Contains(s, "/syswow64/config/systemprofile")
}
