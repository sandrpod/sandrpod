//go:build windows

// Copyright 2026 SandrPod Contributors
// blacklist_windows.go — Windows-specific replacement for the default
// (Unix-only) raw blacklists declared in executor.go.
//
// This file's init() runs BEFORE executor.go:init() because Go's spec orders
// init() within a package by file name alphabetically: "blacklist_windows.go"
// (b) < "executor.go" (e). We exploit that to overwrite rawReadBlacklist /
// rawWriteBlacklist with Windows-appropriate system paths so that
// expandBlacklist (called from executor.go:init) operates on the right set.

package toolbox

import (
	"os"
	"path/filepath"
	"strings"
)

func init() {
	rawReadBlacklist = windowsReadBlacklist()
	rawWriteBlacklist = windowsWriteBlacklist()
}

// windowsReadBlacklist returns paths whose CONTENT is sensitive and must not
// be readable by AI agents (private keys, credential vaults, registry hives).
// Empty entries (env var unset) are filtered out so we never end up blacklisting
// "C:\" by accident.
func windowsReadBlacklist() []string {
	sysroot := os.Getenv("SystemRoot")
	user := os.Getenv("USERPROFILE")
	local := os.Getenv("LOCALAPPDATA")
	roaming := os.Getenv("APPDATA")

	return cleanWinPaths([]string{
		// Registry / SAM hives — owning these is equivalent to owning the host.
		joinIf(sysroot, `System32\config`),
		// SSH private keys.
		joinIf(user, `.ssh`),
		// Windows Credential Manager vaults.
		joinIf(local, `Microsoft\Credentials`),
		joinIf(roaming, `Microsoft\Credentials`),
		joinIf(local, `Microsoft\Vault`),
		joinIf(roaming, `Microsoft\Vault`),
		// DPAPI master keys (used to encrypt browser saved passwords, etc.).
		joinIf(roaming, `Microsoft\Protect`),
	})
}

// windowsWriteBlacklist returns paths that must not be MODIFIED. We avoid
// locking out the entire user profile because legitimate dev work lives in
// Documents / Desktop / Downloads — instead we lock only the sensitive
// subtrees explicitly.
func windowsWriteBlacklist() []string {
	sysroot := os.Getenv("SystemRoot")
	pf := os.Getenv("ProgramFiles")
	pf86 := os.Getenv("ProgramFiles(x86)")
	pd := os.Getenv("ProgramData")
	user := os.Getenv("USERPROFILE")
	local := os.Getenv("LOCALAPPDATA")
	roaming := os.Getenv("APPDATA")

	return cleanWinPaths([]string{
		sysroot, // C:\Windows
		pf,      // C:\Program Files
		pf86,    // C:\Program Files (x86)
		pd,      // C:\ProgramData
		// Same sensitive subtrees as the read blacklist — re-listed so writes
		// are denied even if a read carve-out is added later.
		joinIf(user, `.ssh`),
		joinIf(local, `Microsoft\Credentials`),
		joinIf(roaming, `Microsoft\Credentials`),
		joinIf(local, `Microsoft\Vault`),
		joinIf(roaming, `Microsoft\Vault`),
		joinIf(roaming, `Microsoft\Protect`),
	})
}

// joinIf returns base+`\`+sub only when base is non-empty (env var was set).
// Returning "" for unset envs lets cleanWinPaths drop them.
func joinIf(base, sub string) string {
	if strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(base, sub)
}

// cleanWinPaths normalizes separators and drops empty entries (env vars that
// weren't set on this machine). Without this, a missing env would yield ""
// which prefix-matches every path → total denial of service.
func cleanWinPaths(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
