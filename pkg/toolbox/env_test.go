// Copyright 2026 SandrPod
// Child-environment tests: every spawn site must see the installed extras,
// and an installed variable must replace — not duplicate — the inherited one.

package toolbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestMergeEnvList_OverridesInPlace(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HTTP_PROXY=http://old:1", "HOME=/root"}
	extra := []string{"HTTP_PROXY=http://new:2", "NODE_USE_ENV_PROXY=1"}

	got := mergeEnvList(base, extra)
	joined := strings.Join(got, "\n")

	// The old value must be gone, not merely shadowed: getenv(3) returns the
	// first match, so a duplicate appended after it would lose.
	if strings.Contains(joined, "HTTP_PROXY=http://old:1") {
		t.Errorf("stale HTTP_PROXY survived:\n%s", joined)
	}
	if strings.Count(joined, "HTTP_PROXY=") != 1 {
		t.Errorf("HTTP_PROXY should appear once:\n%s", joined)
	}
	if !strings.Contains(joined, "HTTP_PROXY=http://new:2") {
		t.Errorf("new value missing:\n%s", joined)
	}
	if !strings.Contains(joined, "NODE_USE_ENV_PROXY=1") || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("unrelated variables must be preserved:\n%s", joined)
	}
	// The base order is kept, so the result is stable across runs.
	if got[0] != "PATH=/usr/bin" || got[1] != "HOME=/root" {
		t.Errorf("base order not preserved: %v", got)
	}
}

func TestMergeEnvList_NoExtraReturnsBase(t *testing.T) {
	base := []string{"A=1"}
	if got := mergeEnvList(base, nil); len(got) != 1 || got[0] != "A=1" {
		t.Errorf("mergeEnvList(base, nil) = %v", got)
	}
}

func TestChildEnv_SetAndClear(t *testing.T) {
	t.Setenv("SANDRPOD_TEST_VAR", "inherited")

	SetChildEnv([]string{"SANDRPOD_TEST_VAR=installed", "HTTP_PROXY=http://127.0.0.1:1"})
	defer SetChildEnv(nil)

	env := ChildEnv()
	var sawVar, sawProxy bool
	for _, kv := range env {
		switch kv {
		case "SANDRPOD_TEST_VAR=installed":
			sawVar = true
		case "SANDRPOD_TEST_VAR=inherited":
			t.Errorf("inherited value should have been replaced: %v", env)
		case "HTTP_PROXY=http://127.0.0.1:1":
			sawProxy = true
		}
	}
	if !sawVar || !sawProxy {
		t.Errorf("ChildEnv missing installed variables: %v", env)
	}

	SetChildEnv(nil)
	for _, kv := range ChildEnv() {
		if strings.HasPrefix(kv, "HTTP_PROXY=") {
			t.Errorf("clearing should drop the installed variables, got %q", kv)
		}
	}
}

// The point of ChildEnv is that a spawned process sees it. This runs a real
// shell through the real executor, so a spawn site that reverts to
// os.Environ() fails here rather than in production.
func TestExecutor_SpawnsWithChildEnv(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	SetChildEnv([]string{"SANDRPOD_CHILD_ENV_PROBE=installed"})
	defer SetChildEnv(nil)

	e := NewExecutor()
	res, err := e.Execute(context.Background(), "bash", `printf '%s' "$SANDRPOD_CHILD_ENV_PROBE"`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "installed" {
		t.Fatalf("child saw SANDRPOD_CHILD_ENV_PROBE=%q, want \"installed\"", res.Stdout)
	}
}
