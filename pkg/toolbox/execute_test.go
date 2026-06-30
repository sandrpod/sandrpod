// Copyright 2024 SandrPod
// Execution-path tests for Executor.Execute / ExecuteStream.
//
// These tests spawn real subprocesses but only ever use `bash` (universally
// present on the Unix CI hosts). On a host without bash they skip rather than
// fail.

package toolbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// requireCmd skips the test when the named executable is not on PATH so the
// suite stays green on minimal hosts.
func requireCmd(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%q not found on PATH; skipping", name)
	}
}

// ---------- Execute ----------

func TestExecute_BashEcho_CapturesStdout(t *testing.T) {
	requireCmd(t, "bash")
	e := newTestExecutor(t)
	res, err := e.Execute(context.Background(), "bash", "echo hi-there")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hi-there" {
		t.Errorf("Stdout = %q, want hi-there", res.Stdout)
	}
}

func TestExecute_BashNonZeroExit_ReturnsExitCode(t *testing.T) {
	requireCmd(t, "bash")
	e := newTestExecutor(t)
	res, err := e.Execute(context.Background(), "bash", "exit 3")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestExecute_BashStderr_CapturesStderr(t *testing.T) {
	requireCmd(t, "bash")
	e := newTestExecutor(t)
	res, err := e.Execute(context.Background(), "bash", "echo oops 1>&2")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Stderr, "oops") {
		t.Errorf("Stderr = %q, want to contain oops", res.Stderr)
	}
}

func TestExecute_UnsupportedLanguage_ReturnsError(t *testing.T) {
	e := newTestExecutor(t)
	_, err := e.Execute(context.Background(), "cobol", "DISPLAY 'HI'")
	if err == nil {
		t.Fatal("expected error for unsupported language")
	}
	if !strings.Contains(err.Error(), "unsupported language") {
		t.Errorf("err = %v, want 'unsupported language'", err)
	}
}

// NOTE on the timeout path (ExitCode 124):
// The timeout branch in Executor.ExecuteStream calls cmd.Wait() concurrently
// with the background goroutine that also calls cmd.Wait() (executor.go ~line
// 349 vs ~line 355). os/exec.Cmd.Wait is not safe to invoke twice, so the
// race detector flags it and any -race test exercising the timeout path fails.
// This is a real source bug (documented in the task report), not a test
// problem; we therefore do not drive the timeout path under -race here. The
// non-timeout completion path is covered above.

// ---------- ExecuteStream ----------

func TestExecuteStream_InvokesCallbackForStdout(t *testing.T) {
	requireCmd(t, "bash")
	e := newTestExecutor(t)

	var events []string
	var data []string
	_, err := e.ExecuteStream(context.Background(), "bash", "echo streamed-out", func(event, d string) {
		events = append(events, event)
		data = append(data, d)
	})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	foundStdout := false
	for i, ev := range events {
		if ev == "stdout" && strings.Contains(data[i], "streamed-out") {
			foundStdout = true
		}
	}
	if !foundStdout {
		t.Errorf("no stdout callback with expected data; events=%v data=%v", events, data)
	}
}

func TestExecuteStream_NilCallback_StillReturnsResult(t *testing.T) {
	requireCmd(t, "bash")
	e := newTestExecutor(t)
	res, err := e.ExecuteStream(context.Background(), "bash", "echo no-cb", nil)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if res.Stdout != "no-cb" {
		t.Errorf("Stdout = %q, want no-cb", res.Stdout)
	}
}

// ---------- concurrency limit ----------

func TestExecuteStream_TooManyConcurrent_ReturnsError(t *testing.T) {
	e := newTestExecutor(t)
	// Saturate the running counter past maxRun without spawning processes.
	e.mu.Lock()
	e.running = e.maxRun
	e.mu.Unlock()

	_, err := e.ExecuteStream(context.Background(), "bash", "echo x", nil)
	if err == nil {
		t.Fatal("expected 'too many concurrent executions' error")
	}
	if !strings.Contains(err.Error(), "too many concurrent") {
		t.Errorf("err = %v, want 'too many concurrent executions'", err)
	}
}
