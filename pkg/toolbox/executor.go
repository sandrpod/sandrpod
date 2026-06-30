// Copyright 2024 SandrPod
// Code executor

package toolbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// ErrAccessDenied is returned when a path is rejected by the blacklist policy.
var ErrAccessDenied = errors.New("access denied")

// Executor manages concurrent code execution
type Executor struct {
	mu      sync.RWMutex
	running int
	maxRun  int    // Maximum concurrent executions
	workDir string // Safe root directory for file operations

	// permMgr is the optional permission gate. When non-nil, every file
	// operation that escapes workDir is routed through it (employee gets a
	// desktop consent prompt). Nil = legacy behavior (system blacklist only).
	// Set via SetPermissionManager — typically once during agent startup.
	permMgr *permission.Manager

	// Health-check cache. HealthCheck() probes the runtime by spawning
	// `python3 --version` and `node --version` (~28ms combined); a short TTL
	// collapses repeated probes (e.g. docker-compose healthcheck polling) into
	// one spawn per healthCacheTTL while still re-probing periodically so a
	// broken runtime is eventually surfaced. Guarded by healthMu, independent
	// of mu so the spawn never blocks execution accounting.
	healthMu     sync.Mutex
	healthCache  HealthCheckResult
	healthExpiry time.Time
	healthProbes int // count of real probes (cache misses); observability + tests
}

// healthCacheTTL bounds how stale a cached HealthCheck result may be.
const healthCacheTTL = 10 * time.Second

// SetPermissionManager installs (or replaces) the permission manager.
// Pass nil to disable interactive permission gating entirely. Safe to call
// at any time; in-flight resolves use whichever manager is observed.
func (e *Executor) SetPermissionManager(mgr *permission.Manager) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.permMgr = mgr
}

// GetWorkDirForPermission exposes the executor's workDir so the agent main
// can build the permission.Manager with a matching silent-allow zone.
func (e *Executor) GetWorkDirForPermission() string { return e.workDir }

// PermissionManager returns the currently-installed manager (or nil if
// permission gating is off). The HTTP layer uses this to apply PTY consent
// without re-resolving the path tree.
func (e *Executor) PermissionManager() *permission.Manager {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.permMgr
}

// sandboxSessionContextKey is unexported to prevent collisions with caller-
// defined keys; callers use WithSandboxSession / sessionFromContext.
type sandboxSessionContextKey struct{}

// WithSandboxSession returns a derived context that carries `sessionID`.
// HTTP handlers should call this when they have a sandbox-session id so the
// permission manager can attach session-scoped grants correctly.
func WithSandboxSession(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sandboxSessionContextKey{}, sessionID)
}

// sessionFromContext retrieves the sandbox-session id stored by
// WithSandboxSession, or "" if none is present.
func sessionFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sandboxSessionContextKey{}).(string); ok {
		return v
	}
	return ""
}

// resolveAndAuthorize performs the existing system-blacklist check via
// resolveSafePath, then (if a permission manager is installed) routes the
// resulting absolute path through the consent flow.
//
// Caller is a free-text label surfaced in the prompt body and audit log
// (e.g. "files.read", "files.delete"). It is NOT a security boundary —
// just operator UX.
func (e *Executor) resolveAndAuthorize(ctx context.Context, p string, mode permission.Mode, caller string) (string, error) {
	write := mode == permission.ModeWrite || mode == permission.ModeReadWrite
	safe, err := e.resolveSafePath(p, write)
	if err != nil {
		return "", err
	}

	e.mu.RLock()
	mgr := e.permMgr
	e.mu.RUnlock()
	if mgr == nil {
		return safe, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	decision := mgr.Check(ctx, permission.Request{
		Path:      safe,
		Mode:      mode,
		Caller:    caller,
		SessionID: sessionFromContext(ctx),
	})
	if decision.Action == permission.ActionDeny {
		return "", fmt.Errorf("%w: %s", ErrAccessDenied, decision.Reason)
	}
	return safe, nil
}

// NewExecutor creates a new Executor with default settings
func NewExecutor() *Executor {
	return &Executor{
		maxRun:  10, // Default concurrency limit
		workDir: defaultWorkDir(),
	}
}

// rawReadBlacklist / rawWriteBlacklist are the raw blacklist entries; init() expands them (including symlink resolution).
var rawReadBlacklist = []string{"/proc", "/sys", "/dev", "/boot"}

var rawWriteBlacklist = []string{
	"/etc",
	"/root",
	"/bin",
	"/sbin",
	"/usr",
	"/lib",
	"/lib64",
	"/var/run",
}

// readBlacklist / writeBlacklist are populated by init() with both the raw paths and their
// resolved symlink targets so that restrictions work correctly on macOS (/etc → /private/etc, etc.) and Linux.
var (
	readBlacklist  []string
	writeBlacklist []string
)

func init() {
	readBlacklist = expandBlacklist(rawReadBlacklist)
	writeBlacklist = expandBlacklist(rawWriteBlacklist)
}

// expandBlacklist appends the resolved symlink target for each blacklist path when it differs from the original.
func expandBlacklist(list []string) []string {
	seen := make(map[string]bool, len(list)*2)
	result := make([]string, 0, len(list)*2)
	for _, p := range list {
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil && !seen[resolved] {
			seen[resolved] = true
			result = append(result, resolved)
		}
	}
	return result
}

// isBlacklisted reports whether clean (a normalized absolute path) matches any blacklist prefix.
// Uses filepath.Separator so the prefix match works on both Unix ("/") and Windows ("\").
func isBlacklisted(clean string, list []string) bool {
	sep := string(filepath.Separator)
	for _, prefix := range list {
		if clean == prefix || strings.HasPrefix(clean, prefix+sep) {
			return true
		}
	}
	return false
}

// resolveSymlinks resolves symlinks in a path as much as possible.
// For paths that do not yet exist (e.g. files about to be created), it walks up to
// the nearest existing parent, resolves that, then re-appends the remaining segments
// so that /tmp/foo → /private/tmp/foo (macOS) and similar cases are handled correctly.
func resolveSymlinks(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveSymlinks(parent), filepath.Base(p))
}

// resolveSafePath normalizes a user-supplied path and checks it against the read/write blacklists.
// Relative paths are resolved against workDir; an empty path returns workDir itself.
// When write=true, the write blacklist is also checked (system files must not be modified);
// when write=false, only the read blacklist is checked (kernel/device paths are inaccessible).
func (e *Executor) resolveSafePath(p string, write bool) (string, error) {
	if p == "" {
		return e.workDir, nil
	}
	// Normalize forward slashes to the native separator. On Windows clients
	// commonly send "/c/Users/x/a.txt" or "/foo/bar"; without this conversion
	// filepath.IsAbs returns false for slash-prefixed paths on Windows and
	// the path gets incorrectly joined onto workDir. On Unix FromSlash is a
	// no-op (separator is already "/").
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.workDir, p)
	}
	clean := resolveSymlinks(filepath.Clean(p))

	if isBlacklisted(clean, readBlacklist) {
		return "", fmt.Errorf("%w: %q is a restricted system path", ErrAccessDenied, clean)
	}
	if write && isBlacklisted(clean, writeBlacklist) {
		return "", fmt.Errorf("%w: %q is read-only (system files cannot be modified)", ErrAccessDenied, clean)
	}
	return clean, nil
}

// Execute runs code in the given language and returns the result
func (e *Executor) Execute(ctx context.Context, language, code string) (*ProcessResult, error) {
	return e.ExecuteStream(ctx, language, code, nil)
}

// StreamCallback is a callback for streaming output events
type StreamCallback func(event string, data string)

// ExecuteStream executes code and streams output via an optional callback
func (e *Executor) ExecuteStream(ctx context.Context, language, code string, callback StreamCallback) (*ProcessResult, error) {
	e.mu.Lock()
	if e.running >= e.maxRun {
		e.mu.Unlock()
		return nil, fmt.Errorf("too many concurrent executions")
	}
	e.running++
	mgr := e.permMgr
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.running--
		e.mu.Unlock()
	}()

	// Command policy gate. We pattern-match the submitted code against the
	// deny/warn lists from permissions.json BEFORE spawning the runtime —
	// see pkg/permission/policy.go for the honest scope ("token-level
	// scan, not a real shell parser"). Warn-level hits are recorded for
	// the caller via the StreamCallback so the agent UI / Acme backend
	// can surface them as audit events.
	if mgr != nil {
		dec := mgr.CheckExec(code)
		if callback != nil {
			for _, w := range dec.Warns {
				callback("policy_warn", fmt.Sprintf("⚠️ command %q is on the warn-list (token %q)", w.Command, w.Token))
			}
		}
		if dec.Action == permission.ActionDeny {
			return nil, fmt.Errorf("%w: %s", ErrAccessDenied, dec.Reason)
		}
	}

	start := time.Now()

	var cmd *exec.Cmd
	switch strings.ToLower(language) {
	case "python", "python3":
		cmd = exec.Command(nativePython(), "-c", code)
	case "node", "nodejs":
		cmd = exec.Command("node", "-e", code)
	case "bash", "sh", "shell":
		// On Windows, use PowerShell; on Unix, use /bin/bash.
		shell := nativeShell()
		args := append(nativeShellRunArgs(), prepareExecuteCode(code))
		cmd = exec.Command(shell, args...)
	case "powershell", "pwsh":
		cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", code)
	case "go":
		cmd = exec.Command("go", "run", "-")
		cmd.Stdin = strings.NewReader(code)
	case "ruby":
		cmd = exec.Command("ruby", "-e", code)
	case "perl":
		cmd = exec.Command("perl", "-e", code)
	case "php":
		cmd = exec.Command("php", "-r", code)
	default:
		return nil, fmt.Errorf("unsupported language: %s", language)
	}

	// Set working directory and environment
	cmd.Dir = defaultWorkDir()
	// Inherit the parent process environment so PATH resolves python3, node, etc.
	cmd.Env = os.Environ()
	// Process group isolation (Unix only; no-op on Windows)
	setSysProcAttr(cmd)

	// Attach pipes for streaming output
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	// Ensure a valid context (for timeout handling)
	if ctx == nil {
		ctx = context.Background()
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	// Read stdout and stderr concurrently
	var wg sync.WaitGroup
	var stdout, stderr bytes.Buffer

	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(&stdout, stdoutPipe) //nolint:errcheck
	}()
	go func() {
		defer wg.Done()
		io.Copy(&stderr, stderrPipe) //nolint:errcheck
	}()

	// Wait for completion or context cancellation
	done := make(chan error, 1)
	go func() {
		wg.Wait()
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		// Kill the process, then wait for the single Wait()er goroutine to
		// finish. Blocking on `done` (rather than calling cmd.Wait() again
		// here) avoids a double cmd.Wait — which is undefined — and also
		// guarantees the io.Copy goroutines have stopped writing to the
		// buffers before we read them below.
		killProcess(cmd)
		<-done
		return &ProcessResult{
			ExitCode:  124,
			Stdout:    stdout.String(),
			Stderr:    "Execution cancelled or timed out",
			StartedAt: start,
			EndedAt:   time.Now(),
		}, nil
	case err := <-done:
		ended := time.Now()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}

		// Deliver final output via callback
		if callback != nil {
			if stdout.Len() > 0 {
				callback("stdout", stdout.String())
			}
			if stderr.Len() > 0 {
				callback("stderr", stderr.String())
			}
		}

		return &ProcessResult{
			ExitCode:  exitCode,
			Stdout:    strings.TrimSuffix(stdout.String(), "\n"),
			Stderr:    strings.TrimSuffix(stderr.String(), "\n"),
			StartedAt: start,
			EndedAt:   ended,
		}, nil
	}
}

// HealthCheck verifies that required runtimes are available
// HealthCheck reports runtime availability (Docker/Python/Node). The result is
// cached for healthCacheTTL because probing spawns external processes; rapid
// callers (health-check pollers) reuse the cached result, while the cache still
// refreshes periodically so a runtime that breaks is eventually reflected.
func (e *Executor) HealthCheck() HealthCheckResult {
	e.healthMu.Lock()
	defer e.healthMu.Unlock()

	if !e.healthExpiry.IsZero() && time.Now().Before(e.healthExpiry) {
		return e.healthCache
	}

	e.healthCache = probeHealth()
	e.healthExpiry = time.Now().Add(healthCacheTTL)
	e.healthProbes++
	return e.healthCache
}

// probeHealth performs the actual (cold) runtime probe by spawning version
// checks. Callers should go through HealthCheck to benefit from the TTL cache.
func probeHealth() HealthCheckResult {
	result := HealthCheckResult{
		Docker: true,
	}

	// Check Python
	if err := exec.Command("python3", "--version").Run(); err != nil {
		result.Python = false
		result.Docker = false
	} else {
		result.Python = true
	}

	// Check Node
	if err := exec.Command("node", "--version").Run(); err != nil {
		result.Node = false
	} else {
		result.Node = true
	}

	return result
}

// HealthCheckResult holds the result of a health check
type HealthCheckResult struct {
	Docker bool
	Python bool
	Node   bool
}

// Stats returns current execution statistics
func (e *Executor) Stats() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return map[string]any{
		"running": e.running,
		"max_run": e.maxRun,
	}
}
