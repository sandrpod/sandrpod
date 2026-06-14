package toolbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---------- isBlacklisted ----------

func TestIsBlacklisted_PathUnderBlacklistedPrefix_ReturnsTrue(t *testing.T) {
	list := []string{"/etc", "/usr"}
	cases := []string{
		"/etc/passwd",
		"/etc/ssl/certs/ca.crt",
		"/usr/bin/env",
	}
	for _, p := range cases {
		if !isBlacklisted(p, list) {
			t.Errorf("isBlacklisted(%q, list) = false, want true", p)
		}
	}
}

func TestIsBlacklisted_PathNotUnderAnyPrefix_ReturnsFalse(t *testing.T) {
	list := []string{"/etc", "/usr"}
	cases := []string{
		"/home/user/file.txt",
		"/tmp/workdir",
		"/workspace/code.py",
	}
	for _, p := range cases {
		if isBlacklisted(p, list) {
			t.Errorf("isBlacklisted(%q, list) = true, want false", p)
		}
	}
}

func TestIsBlacklisted_ExactMatch_ReturnsTrue(t *testing.T) {
	list := []string{"/etc", "/proc"}
	if !isBlacklisted("/etc", list) {
		t.Error("isBlacklisted exact match /etc: want true, got false")
	}
	if !isBlacklisted("/proc", list) {
		t.Error("isBlacklisted exact match /proc: want true, got false")
	}
}

func TestIsBlacklisted_EmptyList_ReturnsFalse(t *testing.T) {
	if isBlacklisted("/etc/passwd", nil) {
		t.Error("isBlacklisted with nil list: want false, got true")
	}
	if isBlacklisted("/etc/passwd", []string{}) {
		t.Error("isBlacklisted with empty list: want false, got true")
	}
}

// A path that shares a prefix string but is not a child directory must NOT match.
// e.g. "/etcfoo" should NOT be blocked by the prefix "/etc".
func TestIsBlacklisted_PrefixStringNoSlash_ReturnsFalse(t *testing.T) {
	list := []string{"/etc"}
	if isBlacklisted("/etcfoo", list) {
		t.Error("isBlacklisted(/etcfoo, [/etc]): want false, got true (prefix collision)")
	}
}

// ---------- expandBlacklist ----------

func TestExpandBlacklist_ContainsInputPaths(t *testing.T) {
	input := []string{"/tmp", "/var"}
	result := expandBlacklist(input)

	for _, want := range input {
		if !slices.Contains(result, want) {
			t.Errorf("expandBlacklist: input path %q missing from output %v", want, result)
		}
	}
}

func TestExpandBlacklist_EmptyInput_ReturnsEmpty(t *testing.T) {
	result := expandBlacklist(nil)
	if len(result) != 0 {
		t.Errorf("expandBlacklist(nil): want empty, got %v", result)
	}
	result = expandBlacklist([]string{})
	if len(result) != 0 {
		t.Errorf("expandBlacklist([]): want empty, got %v", result)
	}
}

func TestExpandBlacklist_NoDuplicates(t *testing.T) {
	// Supply two identical entries; the result should contain it only once.
	input := []string{"/tmp", "/tmp"}
	result := expandBlacklist(input)
	count := 0
	for _, p := range result {
		if p == "/tmp" {
			count++
		}
	}
	if count > 1 {
		t.Errorf("expandBlacklist: /tmp appears %d times, want at most 1", count)
	}
}

// ---------- resolveSafePath ----------

// newTempExecutor creates an Executor whose workDir is a temporary directory.
func newTempExecutor(t *testing.T) *Executor {
	t.Helper()
	dir, err := os.MkdirTemp("", "executor-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return &Executor{
		maxRun:  10,
		workDir: dir,
	}
}

func TestResolveSafePath_EmptyPath_ReturnsWorkDir(t *testing.T) {
	e := newTempExecutor(t)
	got, err := e.resolveSafePath("", false)
	if err != nil {
		t.Fatalf("resolveSafePath empty: unexpected error: %v", err)
	}
	// resolveSafePath returns the workDir unchanged for an empty path (before
	// any symlink expansion), so compare against the raw workDir value.
	if got != e.workDir {
		t.Errorf("resolveSafePath empty: got %q, want %q", got, e.workDir)
	}
}

func TestResolveSafePath_SafePathUnderWorkDir_Succeeds(t *testing.T) {
	e := newTempExecutor(t)
	subPath := filepath.Join(e.workDir, "subdir", "file.py")
	got, err := e.resolveSafePath(subPath, false)
	if err != nil {
		t.Fatalf("resolveSafePath safe path: unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, resolveSymlinks(e.workDir)) {
		t.Errorf("resolveSafePath: resolved path %q not under workDir %q", got, e.workDir)
	}
}

func TestResolveSafePath_RelativePath_ResolvesUnderWorkDir(t *testing.T) {
	e := newTempExecutor(t)
	got, err := e.resolveSafePath("script.py", false)
	if err != nil {
		t.Fatalf("resolveSafePath relative: unexpected error: %v", err)
	}
	expected := resolveSymlinks(filepath.Join(e.workDir, "script.py"))
	if got != expected {
		t.Errorf("resolveSafePath relative: got %q, want %q", got, expected)
	}
}

func TestResolveSafePath_PathTraversalIntoBlacklistedDir_IsBlocked(t *testing.T) {
	e := newTempExecutor(t)
	// Build a path that traverses from workDir up to /etc/passwd.
	// filepath.Join cleans ".." components, so construct the absolute path directly.
	// /etc/passwd is on the write blacklist (or /private/etc/passwd on macOS after
	// symlink expansion); either way the access check should deny write access.
	_, err := e.resolveSafePath("/etc/passwd", true)
	if err == nil {
		t.Error("resolveSafePath /etc/passwd write=true: expected access-denied error, got nil")
	}
}

func TestResolveSafePath_WriteToBlacklistedPath_ReturnsError(t *testing.T) {
	e := newTempExecutor(t)
	// /etc/passwd is on the write blacklist (directly or via symlink expansion).
	_, err := e.resolveSafePath("/etc/passwd", true)
	if err == nil {
		t.Error("resolveSafePath /etc/passwd write=true: expected error, got nil")
	}
}

func TestResolveSafePath_ReadOnlyBlacklistedPath_ReturnsError(t *testing.T) {
	e := newTempExecutor(t)
	// /proc is on the read blacklist; even read access should be denied.
	_, err := e.resolveSafePath("/proc/1/maps", false)
	if err == nil {
		t.Error("resolveSafePath /proc/1/maps write=false: expected error, got nil")
	}
}

// ---------- Executor.Stats ----------

func TestExecutorStats_ContainsRequiredKeys(t *testing.T) {
	e := NewExecutor()
	stats := e.Stats()
	if stats == nil {
		t.Fatal("Stats: returned nil map")
	}
	if _, ok := stats["running"]; !ok {
		t.Error("Stats: missing key 'running'")
	}
	if _, ok := stats["max_run"]; !ok {
		t.Error("Stats: missing key 'max_run'")
	}
}

func TestExecutorStats_InitialValuesAreZeroAndTen(t *testing.T) {
	e := NewExecutor()
	stats := e.Stats()

	if running, ok := stats["running"].(int); !ok || running != 0 {
		t.Errorf("Stats['running']: want 0, got %v", stats["running"])
	}
	if maxRun, ok := stats["max_run"].(int); !ok || maxRun != 10 {
		t.Errorf("Stats['max_run']: want 10, got %v", stats["max_run"])
	}
}

// ---------- Executor.HealthCheck ----------

func TestExecutorHealthCheck_ReturnsHealthCheckResult(t *testing.T) {
	e := NewExecutor()
	// HealthCheck runs external commands; we just verify the call returns a
	// well-formed HealthCheckResult struct (no panic, no error return).
	result := e.HealthCheck()
	// The Healthy-equivalent fields are Bool: Docker/Python/Node.
	// On the CI machine at least one combination is valid; we only assert the
	// struct is populated (zero value is a valid false state).
	_ = result.Docker
	_ = result.Python
	_ = result.Node
}

func TestExecutorHealthCheck_StructFieldsAreBool(t *testing.T) {
	e := NewExecutor()
	result := e.HealthCheck()
	// Compile-time: if the fields changed type the next lines would not compile.
	var _ bool = result.Docker
	var _ bool = result.Python
	var _ bool = result.Node
}

// HealthCheck spawns `python3 --version` and `node --version` on every call
// (~28ms). These tests pin the TTL cache that collapses repeated probes.

func TestExecutorHealthCheck_CachesWithinTTL(t *testing.T) {
	e := NewExecutor()
	r1 := e.HealthCheck()
	r2 := e.HealthCheck()
	if e.healthProbes != 1 {
		t.Fatalf("expected exactly 1 probe within TTL, got %d", e.healthProbes)
	}
	if r1 != r2 {
		t.Fatalf("cached result differs from first: %+v vs %+v", r1, r2)
	}
}

func TestExecutorHealthCheck_ReprobesAfterTTLExpiry(t *testing.T) {
	e := NewExecutor()
	_ = e.HealthCheck() // probe #1, populates cache

	// Force the cache to expire without sleeping the full TTL.
	e.healthMu.Lock()
	e.healthExpiry = time.Now().Add(-time.Second)
	e.healthMu.Unlock()

	_ = e.HealthCheck() // should re-probe
	if e.healthProbes != 2 {
		t.Fatalf("expected re-probe after TTL expiry, got %d probes", e.healthProbes)
	}
}
