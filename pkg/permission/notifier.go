// Copyright 2026 SandrPod
// Notifier abstraction.
//
// The permission manager is GUI-agnostic: it only knows how to ask "Notifier,
// please show this prompt and tell me what the user clicked". Concrete
// implementations live in pkg/notify/* and are wired in by the agent main.
//
// In Sprint 1 the macOS implementation calls osascript directly from the
// sandrpod-agent process. In Sprint 2 it will be replaced by an IPC client
// that talks to a separate sandrpod-tray binary (so the daemon and the GUI
// can have independent process lifetimes — required for launchd LaunchAgent
// vs LaunchDaemon split, and for clean kills/upgrades).

package permission

import "context"

// Notifier presents a permission request to the human in front of the
// computer and returns their choice.
//
// Contract:
//   - Implementations MUST be safe for concurrent use; the manager may
//     receive overlapping requests from multiple sandbox sessions.
//   - Implementations MUST honor ctx; on ctx.Err() they should return
//     PromptTimeout (not a Go error) so the manager can fail-close.
//   - The string error return is reserved for *infrastructure* failures
//     (e.g. osascript binary missing). A user clicking "deny" is a normal
//     PromptDeny response, not an error.
type Notifier interface {
	Ask(ctx context.Context, req Request) (PromptResponse, error)
}

// NopNotifier denies every prompt. Used when the agent runs in a headless
// environment with no GUI available — fail-close by default. Operators who
// truly want headless allow-all must explicitly install AlwaysAllowNotifier.
type NopNotifier struct{}

// Ask always returns PromptDeny.
func (NopNotifier) Ask(ctx context.Context, req Request) (PromptResponse, error) {
	return PromptDeny, nil
}

// AlwaysAllowNotifier returns PromptAllowOnce for every prompt. Useful for
// tests and for explicitly-headless production setups that intentionally
// disable interactive prompting (rare; document the risk in the operator
// runbook).
type AlwaysAllowNotifier struct{}

// Ask always returns PromptAllowOnce.
func (AlwaysAllowNotifier) Ask(ctx context.Context, req Request) (PromptResponse, error) {
	return PromptAllowOnce, nil
}
