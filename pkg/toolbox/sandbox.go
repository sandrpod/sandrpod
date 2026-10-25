// Copyright 2026 SandrPod Contributors
// OS-sandbox plumbing for spawned processes.
//
// The egress proxy filters network calls; the permission gate filters file
// calls that go through the toolbox API. Neither sees a process that calls
// open(2) or connect(2) directly, which is what Execute and the PTY are for.
// When the agent installs a wrapper here, every process the toolbox starts is
// launched inside the platform's OS sandbox (bubblewrap on Linux, Seatbelt on
// macOS) with the same policy: read-only root, writable work tree, hard-locked
// paths masked.
//
// It lives next to ChildEnv for the same reason: there is exactly one place a
// spawn site can forget to consult, so there is exactly one place it has to.
package toolbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/sandrpod/sandrpod/pkg/sandbox"
)

var (
	sandboxMu         sync.RWMutex
	sandboxWrapper    sandbox.Wrapper
	sandboxPolicy     sandbox.Policy
	sandboxPolicyFunc func() sandbox.Policy
)

// SetSandbox installs (or clears, with a nil wrapper) the OS sandbox applied to
// every process the toolbox spawns.
//
// The policy passed here is the fallback; when a policy function is installed
// (SetSandboxPolicyFunc) it is recomputed for every spawn instead.
func SetSandbox(w sandbox.Wrapper, policy sandbox.Policy) {
	sandboxMu.Lock()
	defer sandboxMu.Unlock()
	sandboxWrapper = w
	sandboxPolicy = policy
	if w == nil {
		sandboxPolicyFunc = nil
	}
}

// SetSandboxPolicyFunc installs a policy *producer* instead of a fixed policy,
// called once per spawn. It exists because the policy is not static:
//
//   - the write-deny walk only sees paths that exist, and `git init` inside the
//     sandbox creates .git/hooks after startup — a snapshot would leave that
//     hook writable, which is the exact vector the list is for;
//   - a hardlock added or unlocked at runtime must reach the kernel too, or the
//     gate and the sandbox disagree about the same path.
func SetSandboxPolicyFunc(f func() sandbox.Policy) {
	sandboxMu.Lock()
	defer sandboxMu.Unlock()
	sandboxPolicyFunc = f
}

// SandboxName reports the installed backend ("bwrap", "seatbelt", or "" for
// none), for the startup log and the tooling that reads it.
func SandboxName() string {
	sandboxMu.RLock()
	defer sandboxMu.RUnlock()
	if sandboxWrapper == nil {
		return ""
	}
	return sandboxWrapper.Name()
}

// wrapSandbox rewrites cmd to run inside the installed sandbox, if any.
// Called at every spawn site immediately before Start.
func wrapSandbox(cmd *exec.Cmd) error {
	sandboxMu.RLock()
	w, policy := sandboxWrapper, sandboxPolicy
	provider := sandboxPolicyFunc
	sandboxMu.RUnlock()
	if w == nil {
		return nil
	}
	if provider != nil {
		// Recomputed per spawn: the write-deny walk only sees paths that exist,
		// and `git init` inside the sandbox creates .git/hooks after startup.
		policy = provider()
	}
	path, args, err := sandbox.Apply(w, policy, cmd.Path, cmd.Args[1:])
	if err != nil {
		return fmt.Errorf("os sandbox: %w", err)
	}
	// execve does no PATH lookup, and cmd.Path is used verbatim. A wrapper that
	// hands back a bare name would fail at exec time with a confusing ENOENT.
	if !strings.ContainsRune(path, os.PathSeparator) {
		if resolved, lerr := exec.LookPath(path); lerr == nil {
			path = resolved
		}
	}
	cmd.Path = path
	cmd.Args = append([]string{path}, args...)
	return nil
}
