// Copyright 2026 SandrPod
// First-run seeding of high-risk hardlock entries.
//
// On the very first launch of sandrpod-tray (i.e. when permissions.json
// doesn't exist yet), we plant a curated list of "you almost certainly do
// not want the AI in here" paths as hardlocks. The employee can still
// unlock any of them via `sandrpod-tray unlock <path> --i-understand-the-risk`,
// but the bar is intentionally high — it requires command-line action plus
// an explicit acknowledgement flag.
//
// What goes here?
//   - Authentication material (SSH keys, cloud creds, browser autofill DBs)
//   - Personal communication archives (Messages.app, browser history)
//   - Mailboxes
// Notably we DO NOT seed `~/Documents`, `~/Downloads`, `~/Desktop`,
// `~/code`, etc. — those are user-driven workspaces where AI assistance is
// the primary value, and we want first-touch consent there, not a wall.

package permission

import (
	"runtime"
	"time"
)

// DefaultHardlockSeeds returns the OS-appropriate set of hardlock paths to
// install on first run. Paths use "~" for home-relative entries; the manager
// expands them at lookup time.
func DefaultHardlockSeeds() []Rule {
	common := []string{
		"~/.ssh",
		"~/.aws",
		"~/.gnupg",
		"~/.kube",
		"~/.config/gh",
		"~/.docker",
		// sandrpod's own security state: the gate's rules, MCP grants, the
		// audit trail, and persisted OAuth tokens. Hardlocking these stops the
		// AI from rewriting its own permissions / grants or deleting audit
		// evidence via the (gated) file API. Note: this does NOT cover the
		// arbitrary-code path — `/process` runs open()/os.Remove() directly,
		// outside the gate — but it closes the file-API vector. mcp.json (the
		// operator-managed config) is deliberately left writable.
		"~/.sandrpod/permissions.json",
		"~/.sandrpod/mcp_grants.json",
		"~/.sandrpod/audit",
		"~/.sandrpod/oauth",
	}

	var osSpec []string
	switch runtime.GOOS {
	case "darwin":
		osSpec = []string{
			"~/Library/Keychains",
			"~/Library/Application Support/Google/Chrome",
			"~/Library/Application Support/Firefox",
			"~/Library/Application Support/com.apple.sharedfilelist",
			"~/Library/Mail",
			"~/Library/Messages",
			"~/Library/Cookies",
		}
	case "linux":
		osSpec = []string{
			"~/.mozilla",
			"~/.config/google-chrome",
			"~/.config/chromium",
			"~/.local/share/keyrings",
			"~/.thunderbird",
		}
	case "windows":
		// Windows uses backslashes and %APPDATA%; the file format expects
		// forward-slash paths since we expand "~" at runtime. Keep this
		// minimal for now — Sprint 4 will refine it alongside the
		// Windows prompter implementation.
		osSpec = []string{
			"~/AppData/Roaming/Microsoft/Crypto",
			"~/AppData/Local/Google/Chrome",
			"~/AppData/Local/Microsoft/Edge",
		}
	}

	now := time.Now()
	out := make([]Rule, 0, len(common)+len(osSpec))
	for _, p := range common {
		out = append(out, Rule{Path: p, Mode: "deny", Scope: ScopeHardlock, GrantedAt: now,
			Note: "default seed: authentication material"})
	}
	for _, p := range osSpec {
		out = append(out, Rule{Path: p, Mode: "deny", Scope: ScopeHardlock, GrantedAt: now,
			Note: "default seed: personal data (OS-specific)"})
	}
	return out
}

// SeedHardlocksIfEmpty installs DefaultHardlockSeeds() into store iff the
// store currently has zero rules at all (covers the genuine first-run case
// without overwriting an employee-curated state).
//
// Tilde paths are stored verbatim — the manager expands them at lookup time
// using the current user's home dir, so the same permissions.json is correct
// after `mv ~/Foo ~/Bar` or running as a different user (the latter shouldn't
// happen, but defense in depth).
//
// Returns the number of rules successfully added.
func SeedHardlocksIfEmpty(store *Store) (int, error) {
	snap := store.Snapshot()
	if len(snap.Rules) > 0 {
		return 0, nil
	}
	added := 0
	for _, r := range DefaultHardlockSeeds() {
		if err := store.AddHardlock(r.Path); err != nil {
			// Don't abort on a single failure — keep going so a single
			// disk hiccup doesn't leave the user with no protection at all.
			continue
		}
		added++
	}
	return added, nil
}
