package toolbox

import (
	"os/exec"
	"testing"
)

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

func TestKernelManager_StatefulExecution(t *testing.T) {
	requirePython(t)
	m := NewKernelManager("python3")
	defer m.CloseAll()

	// State persists across executions in the same context.
	if _, err := m.Execute("ctx1", "x = 1"); err != nil {
		t.Fatal(err)
	}
	res, err := m.Execute("ctx1", "x += 1\nx")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "2" {
		t.Errorf("stateful result: got Text=%q, want \"2\" (stderr=%q err=%q)", res.Text, res.Stderr, res.Error)
	}

	// stdout is captured.
	res, _ = m.Execute("ctx1", "print('hello')")
	if res.Stdout != "hello\n" {
		t.Errorf("stdout: got %q", res.Stdout)
	}

	// A different context has its own namespace (x undefined → error).
	res, _ = m.Execute("ctx2", "x")
	if res.Error == "" {
		t.Errorf("ctx2 should not see ctx1's x; got %+v", res)
	}
}

func TestKernelManager_ErrorCapture(t *testing.T) {
	requirePython(t)
	m := NewKernelManager("python3")
	defer m.CloseAll()
	res, err := m.Execute("e", "1/0")
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == "" {
		t.Error("expected a traceback for ZeroDivisionError")
	}
}

func TestKernelManager_Close(t *testing.T) {
	requirePython(t)
	m := NewKernelManager("python3")
	_, _ = m.Execute("c", "y = 5")
	m.Close("c")
	// After close, a fresh kernel starts → y is gone.
	res, _ := m.Execute("c", "y")
	if res.Error == "" {
		t.Error("closed context should reset state")
	}
	m.CloseAll()
}
