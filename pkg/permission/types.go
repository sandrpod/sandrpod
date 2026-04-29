// Copyright 2026 SandrPod
// Permission system data model.
//
// Defines the wire-format that lives in ~/.sandrpod/permissions.json and the
// in-memory request/decision types used across the manager / notifier / store.
//
// Design constraints:
//   - JSON file is a stable contract (a future tray GUI must round-trip it).
//   - Paths in the file may contain "~" (expanded at load time only); decisions
//     always operate on absolute, symlink-resolved paths.
//   - Hardlock rules survive GUI edits — they can only be removed by the
//     command-line `sandrpod-tray unlock <path> --i-understand-the-risk` path.

package permission

import (
	"time"
)

// Mode is the access type being requested or granted.
type Mode string

const (
	ModeRead    Mode = "r"
	ModeWrite   Mode = "w"
	ModeReadWrite Mode = "rw"
	// ModeExec is reserved for future PTY/exec session-level prompts.
	ModeExec Mode = "x"
)

// Allows reports whether `granted` covers `requested`.
//   - rw covers r, w, rw
//   - r covers r only; w covers w only
func (granted Mode) Allows(requested Mode) bool {
	if granted == ModeReadWrite {
		return requested == ModeRead || requested == ModeWrite || requested == ModeReadWrite
	}
	return granted == requested
}

// RuleScope defines how long a granted rule lives.
type RuleScope string

const (
	// ScopePermanent rules persist in permissions.json until the employee revokes.
	ScopePermanent RuleScope = "permanent"
	// ScopeSession rules live in memory + JSON for a single sandbox session.
	ScopeSession RuleScope = "session"
	// ScopeHardlock paths are forbidden until manually unlocked via CLI.
	ScopeHardlock RuleScope = "hardlock"
)

// Action is the outcome of a permission check.
type Action string

const (
	ActionAllow Action = "allow" // request proceeds
	ActionDeny  Action = "deny"  // request rejected; surfaced as ErrAccessDenied
	ActionAsk   Action = "ask"   // (internal) — the manager will route to the notifier
)

// Rule is a persisted permission grant or hard-lock entry.
type Rule struct {
	Path      string    `json:"path"`            // may include "~"
	Mode      Mode      `json:"mode"`            // r / w / rw / deny (for hardlock entries we use Mode="deny")
	Scope     RuleScope `json:"scope"`           // permanent | hardlock | session
	GrantedAt time.Time `json:"granted_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"` // session scope only
	SessionID string    `json:"session_id,omitempty"` // session scope only
	Note      string    `json:"note,omitempty"`       // free-text employee note
}

// CommandPolicy controls which command names sandrpod will refuse to launch
// inside exec/PTY. Names are matched against `argv[0]` basename only.
type CommandPolicy struct {
	Deny []string `json:"deny,omitempty"`
	Warn []string `json:"warn,omitempty"`
}

// Snapshot is the full on-disk shape of permissions.json.
//
// File location: $HOME/.sandrpod/permissions.json (chmod 600).
type Snapshot struct {
	Version       int           `json:"version"` // current = 1
	User          string        `json:"user,omitempty"`
	WorkDir       string        `json:"work_dir,omitempty"`
	Rules         []Rule        `json:"rules"`
	SessionGrants []Rule        `json:"session_grants,omitempty"`
	CommandPolicy CommandPolicy `json:"command_policy,omitempty"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// Request is what the manager evaluates.
//
// Path must be absolute and symlink-resolved; the caller (executor) is
// responsible for that — the manager does not normalize.
type Request struct {
	Path     string
	Mode     Mode
	Reason   string // optional: human-readable explanation surfaced in the prompt
	Caller   string // optional: e.g. "files.read", "exec", "pty"
	SessionID string // optional: sandbox session id
}

// Decision is what the manager returns to the executor.
type Decision struct {
	Action Action
	Reason string // populated on Deny so the executor can surface a meaningful error
}

// PromptResponse is what a Notifier returns from Ask().
//
// The Notifier converts a desktop button click into one of these values.
// Persistence (writing permanent / session rules to disk) is handled by the
// manager — the notifier is purely UI.
type PromptResponse string

const (
	// PromptAllowOnce — allow this request only; do not persist.
	PromptAllowOnce PromptResponse = "allow_once"
	// PromptAllowSession — allow until session ends.
	PromptAllowSession PromptResponse = "allow_session"
	// PromptAllowPermanent — write a permanent rule.
	PromptAllowPermanent PromptResponse = "allow_permanent"
	// PromptDeny — refuse this request only.
	PromptDeny PromptResponse = "deny"
	// PromptTimeout — UI gave no answer within deadline; fail-close.
	PromptTimeout PromptResponse = "timeout"
)
