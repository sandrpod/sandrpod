// Copyright 2026 SandrPod
// Permission decision engine.
//
// Check() is the single entry point called by Executor.resolveSafePath after
// the existing system-path blacklist passes. The flow is:
//
//   1. Path under work_dir?      → ALLOW (silent, no prompt)
//   2. Path under hardlock entry? → DENY  (silent, audit only)
//   3. Permanent rule covers it?  → ALLOW
//   4. Live session grant covers? → ALLOW
//   5. Otherwise                  → ASK the notifier; persist on grant
//
// This mirrors macOS's TCC model: most things are quietly allowed inside the
// sandbox root, sensitive paths require explicit consent, and a small
// hard-locked set can never be granted via the GUI.

package permission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultPromptDeadline is the deadline for any single notifier prompt.
// Keep it short — if the human ignores the dialog we want to fail-close
// rather than block the agent forever.
const DefaultPromptDeadline = 30 * time.Second

// AuditSink is the minimal interface Manager uses to record decisions.
// We deliberately do NOT import pkg/audit here — that would make pkg/audit
// reachable from anything that imports pkg/permission, and we want audit
// to remain an opt-in observer rather than a core dependency.
//
// pkg/audit.Recorder satisfies this trivially via a thin adapter the agent
// main wires in (see cmd/agent/main.go).
type AuditSink interface {
	Record(source, decision, path, mode, caller, sessionID, reason, matchedCommand string)
}

// noopAuditSink is the zero-value behavior when no sink is installed.
type noopAuditSink struct{}

func (noopAuditSink) Record(string, string, string, string, string, string, string, string) {}

// Manager evaluates Requests against the persisted Snapshot and an optional
// in-process Notifier. It is safe for concurrent use.
type Manager struct {
	store    *Store
	notifier Notifier
	workDir  string
	homeDir  string
	audit    AuditSink

	// promptMu serializes prompts so the human only sees one dialog at a time
	// even if multiple sandbox sessions hit unmapped paths simultaneously.
	// Without this, osascript on macOS happily stacks dialogs and the user
	// gets a confusing avalanche of unrelated questions.
	promptMu sync.Mutex
}

// SetAuditSink installs (or replaces) the audit sink. Pass nil to disable.
// Safe to call any time; in-flight Check calls pick up the new sink on
// their next decision write.
func (m *Manager) SetAuditSink(s AuditSink) {
	if s == nil {
		s = noopAuditSink{}
	}
	m.audit = s
}

// Options configures a new Manager.
type Options struct {
	Store    *Store    // required
	Notifier Notifier  // required (use NopNotifier for fail-close headless mode)
	WorkDir  string    // sandbox root — silent allow inside this tree
	HomeDir  string    // employee home dir, used to expand "~" in stored rules
}

// NewManager constructs a Manager. The caller owns the Store and Notifier
// lifecycles (Manager does not close them).
func NewManager(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("permission.NewManager: Store is required")
	}
	if opts.Notifier == nil {
		return nil, errors.New("permission.NewManager: Notifier is required (pass NopNotifier for fail-close)")
	}
	wd, err := canonicalize(opts.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("permission.NewManager: bad work_dir: %w", err)
	}
	hd := opts.HomeDir
	if hd == "" {
		hd, _ = os.UserHomeDir()
	}
	return &Manager{
		store:    opts.Store,
		notifier: opts.Notifier,
		workDir:  wd,
		homeDir:  hd,
		audit:    noopAuditSink{},
	}, nil
}

// recordDecision is a single chokepoint for audit emission so we can never
// forget a code path.
func (m *Manager) recordDecision(source string, dec Decision, req Request) {
	if m.audit == nil {
		return
	}
	decisionStr := string(dec.Action)
	if decisionStr == "" {
		decisionStr = "unknown"
	}
	m.audit.Record(source, decisionStr, req.Path, string(req.Mode), req.Caller, req.SessionID, dec.Reason, "")
}

// checkInternal is the original decision logic; Check wraps it to emit one
// audit record per call regardless of which branch fired.
func (m *Manager) checkInternal(ctx context.Context, req Request) Decision {
	return m.checkBody(ctx, req)
}

// Check evaluates req and returns Allow / Deny. Any disk writes (persisting
// new permanent or session grants) happen synchronously before Check returns.
//
// Defensive contract: even though resolveSafePath in the toolbox already
// canonicalizes paths before calling Check, we re-canonicalize here so that
// callers / tests that pass a non-resolved path (e.g. a path under a symlink
// like macOS's /var → /private/var) still get correct work_dir matching.
func (m *Manager) Check(ctx context.Context, req Request) Decision {
	dec := m.checkBody(ctx, req)
	m.recordDecision("path", dec, req)
	return dec
}

// checkBody contains the original decision tree without audit emission so
// Check can record exactly once. Splitting this out keeps the decision
// logic readable and avoids the "did I forget to emit on this branch?"
// failure mode that creeps in when audit is sprinkled inline.
func (m *Manager) checkBody(ctx context.Context, req Request) Decision {
	if req.Path == "" {
		return Decision{Action: ActionDeny, Reason: "empty path"}
	}
	if canonical, err := canonicalize(req.Path); err == nil {
		req.Path = canonical
	}

	// 1. work_dir is the silent-allow zone.
	if m.workDir != "" && pathInside(req.Path, m.workDir) {
		return Decision{Action: ActionAllow}
	}

	// 2. Hardlock check — must run before any allow-rule lookup so that an
	//    accidentally-permanent rule on a hardlocked path can never sneak in.
	snap := m.store.Snapshot()
	if hit := matchHardlock(req.Path, snap.Rules, m.homeDir); hit != nil {
		return Decision{
			Action: ActionDeny,
			Reason: fmt.Sprintf("path %q is hard-locked (use `sandrpod-tray unlock` to enable)", req.Path),
		}
	}

	// 3. Permanent rule check.
	if matchAllow(req, snap.Rules, m.homeDir) {
		return Decision{Action: ActionAllow}
	}

	// 4. Live session grant check (with TTL).
	if matchSessionAllow(req, snap.SessionGrants, m.homeDir, time.Now()) {
		return Decision{Action: ActionAllow}
	}

	// 5. Ask the human.
	return m.askAndPersist(ctx, req)
}

// askAndPersist routes to the notifier, persists the response if applicable,
// and returns the resulting Decision.
func (m *Manager) askAndPersist(ctx context.Context, req Request) Decision {
	m.promptMu.Lock()
	defer m.promptMu.Unlock()

	// Re-check rules after acquiring the lock — another concurrent Check()
	// may have just persisted a grant covering this path.
	snap := m.store.Snapshot()
	if matchAllow(req, snap.Rules, m.homeDir) ||
		matchSessionAllow(req, snap.SessionGrants, m.homeDir, time.Now()) {
		return Decision{Action: ActionAllow}
	}

	promptCtx, cancel := context.WithTimeout(ctx, DefaultPromptDeadline)
	defer cancel()

	resp, err := m.notifier.Ask(promptCtx, req)
	if err != nil {
		return Decision{
			Action: ActionDeny,
			Reason: fmt.Sprintf("permission prompt failed: %v", err),
		}
	}

	switch resp {
	case PromptAllowOnce:
		return Decision{Action: ActionAllow}

	case PromptAllowSession:
		// Session grants need a SessionID to be useful; if the caller didn't
		// provide one (bare exec/files API outside a session) we degrade to
		// allow-once rather than refusing.
		if req.SessionID == "" {
			return Decision{Action: ActionAllow}
		}
		_ = m.store.AddSessionRule(Rule{
			Path:      req.Path,
			Mode:      req.Mode,
			SessionID: req.SessionID,
			ExpiresAt: time.Now().Add(8 * time.Hour),
		})
		return Decision{Action: ActionAllow}

	case PromptAllowPermanent:
		_ = m.store.AddPermanentRule(Rule{
			Path: req.Path,
			Mode: req.Mode,
		})
		return Decision{Action: ActionAllow}

	case PromptDeny:
		return Decision{Action: ActionDeny, Reason: "denied by user"}

	case PromptTimeout:
		return Decision{Action: ActionDeny, Reason: "permission prompt timed out (fail-close)"}

	default:
		return Decision{Action: ActionDeny, Reason: fmt.Sprintf("unknown prompt response %q", resp)}
	}
}

// ExecDecision is the result of CheckExec — richer than Decision because
// callers want to log warn-level hits even when proceeding.
type ExecDecision struct {
	Action Action       // ActionAllow or ActionDeny
	Reason string       // populated on Deny
	Warns  []CommandHit // populated whether allowed or denied; informational
	Deny   *CommandHit  // populated on Deny; the hit that triggered the block
}

// CheckExec applies command policy to a piece of code about to be executed.
//
// It does NOT consult the file path tree (Check is the right method for
// that). Callers that need both — file-level resolveSafePath authorization
// AND command-level policy — must call both.
//
// The policy itself comes from the persisted store; if the store has no
// deny/warn entries (e.g. a fresh install with no seeds), CheckExec is a
// no-op pass.
//
// Audit emission: warn-level hits are recorded individually so a single
// piece of code triggering multiple warns produces one audit row per
// matched command. Deny is recorded once.
func (m *Manager) CheckExec(code string) ExecDecision {
	snap := m.store.Snapshot()
	warns, deny, hasDeny := CheckCommandPolicy(snap.CommandPolicy, code)

	// Emit warns regardless of final decision so admins see the full
	// risk surface even when the same code is also blocked by a deny.
	for _, w := range warns {
		if m.audit != nil {
			m.audit.Record("exec", "warn", "", "", "", "", w.Token, w.Command)
		}
	}

	if hasDeny && deny != nil {
		dec := ExecDecision{
			Action: ActionDeny,
			Reason: fmt.Sprintf("command %q is denied by policy (matched token %q)", deny.Command, deny.Token),
			Warns:  warns,
			Deny:   deny,
		}
		if m.audit != nil {
			m.audit.Record("exec", "deny", "", "", "", "", dec.Reason, deny.Command)
		}
		return dec
	}
	if m.audit != nil {
		m.audit.Record("exec", "allow", "", "", "", "", "", "")
	}
	return ExecDecision{Action: ActionAllow, Warns: warns}
}

// CheckPTY asks the human for consent before opening an interactive PTY
// session. Unlike file-level Check, this is always asked — there is no
// "silent allow" zone for shells, since by definition a PTY can do anything
// inside whatever paths the existing rules already allow.
//
// We surface ModeExec on the prompt so the dialog wording can differentiate
// from a plain file read.
func (m *Manager) CheckPTY(ctx context.Context, sandboxName, sessionID string) Decision {
	// PTY is always silent-allow when the manager is uninstalled
	// (off mode in the agent). This branch keeps callers ergonomic.
	if m == nil {
		return Decision{Action: ActionAllow}
	}
	// We could short-circuit "auto-allow if a session-grant for this
	// sessionID already approved a PTY". For Sprint 3 we keep it simple:
	// every PTY open prompts. Sprint 4 may add a session-grant cache.
	req := Request{
		Path:      "PTY:" + sandboxName,
		Mode:      ModeExec,
		Caller:    "pty.open",
		SessionID: sessionID,
		Reason:    "AI 请求打开交互式终端会话（一旦允许，AI 在该会话中可以执行任意 shell 命令，仅受路径授权和命令策略限制）",
	}

	m.promptMu.Lock()
	defer m.promptMu.Unlock()

	promptCtx, cancel := context.WithTimeout(ctx, DefaultPromptDeadline)
	defer cancel()

	resp, err := m.notifier.Ask(promptCtx, req)
	if err != nil {
		return Decision{Action: ActionDeny, Reason: fmt.Sprintf("pty consent prompt failed: %v", err)}
	}
	var dec Decision
	switch resp {
	case PromptAllowOnce, PromptAllowSession, PromptAllowPermanent:
		// We don't persist permanent PTY grants — that would let an AI
		// open shells silently forever. PTY consent is per-open.
		dec = Decision{Action: ActionAllow}
	case PromptDeny:
		dec = Decision{Action: ActionDeny, Reason: "PTY denied by user"}
	case PromptTimeout:
		dec = Decision{Action: ActionDeny, Reason: "PTY consent prompt timed out (fail-close)"}
	default:
		dec = Decision{Action: ActionDeny, Reason: fmt.Sprintf("unknown prompt response %q", resp)}
	}
	m.recordDecision("pty", dec, req)
	return dec
}

// suppress unused warning when audit logic is not yet active; checkInternal
// is reserved for future paths that want to bypass audit (none today).
var _ = (*Manager).checkInternal

// ---- helpers ----

// pathInside reports whether `child` is `parent` itself or a path beneath it.
// Both inputs must be clean, absolute, and ideally symlink-resolved.
func pathInside(child, parent string) bool {
	if parent == "" {
		return false
	}
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// expandTilde replaces a leading "~" in `p` with `home`.
func expandTilde(p, home string) string {
	if home == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p // "~user/..." style not supported; return as-is so it won't match
}

// canonicalize returns a clean absolute path with symlinks resolved as far
// as possible.
//
// For non-existent leaf paths (e.g. a file the AI is about to create), we
// recurse up to the nearest existing parent, resolve that, then re-append
// the trailing segments. This matches the executor's resolveSymlinks()
// helper and is essential on macOS where t.TempDir() returns
// /var/folders/... which is a symlink to /private/var/folders/... — without
// this walk, work_dir matching fails for any not-yet-existent file.
func canonicalize(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return resolveAncestors(filepath.Clean(abs)), nil
}

// resolveAncestors walks up p until it finds an existing path it can
// EvalSymlinks, then re-joins the unresolved suffix.
func resolveAncestors(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveAncestors(parent), filepath.Base(p))
}

// matchHardlock returns the first hardlock rule covering `path`, or nil.
func matchHardlock(path string, rules []Rule, home string) *Rule {
	for i := range rules {
		r := &rules[i]
		if r.Scope != ScopeHardlock {
			continue
		}
		rp := expandTilde(r.Path, home)
		if pathInside(path, rp) {
			return r
		}
	}
	return nil
}

// matchAllow reports whether any permanent allow-rule covers req.
func matchAllow(req Request, rules []Rule, home string) bool {
	for _, r := range rules {
		if r.Scope != ScopePermanent {
			continue
		}
		if r.Mode == "deny" {
			continue // explicit deny rules don't grant access (handled separately)
		}
		if !r.Mode.Allows(req.Mode) {
			continue
		}
		if pathInside(req.Path, expandTilde(r.Path, home)) {
			return true
		}
	}
	return false
}

// matchSessionAllow reports whether any non-expired session grant covers req.
func matchSessionAllow(req Request, grants []Rule, home string, now time.Time) bool {
	for _, r := range grants {
		if !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) {
			continue
		}
		if r.SessionID != "" && req.SessionID != "" && r.SessionID != req.SessionID {
			continue
		}
		if !r.Mode.Allows(req.Mode) {
			continue
		}
		if pathInside(req.Path, expandTilde(r.Path, home)) {
			return true
		}
	}
	return false
}
