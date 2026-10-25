// Copyright 2026 SandrPod Contributors

package sandbox

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A PTY session must survive the wrapper. pty.Start sets Setsid on the command
// it is given, so if the wrapper is started that way and insists on its own
// setsid(), every interactive session would fail to start.
func TestBwrap_StartsUnderAPTY(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	w := &Bwrap{Path: "bwrap"}
	if err := w.Available(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}
	work, err := os.MkdirTemp("/var/tmp", "sandrpod-pty-")
	if err != nil {
		t.Skipf("no scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })

	path, args, err := w.Command(Policy{WorkDir: work}, "/bin/sh", []string{"-c", "echo pty-ok"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	cmd := exec.Command(path, args...)
	f, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("no pty available here: %v", err)
	}
	defer f.Close()

	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(f)
		out <- string(b)
	}()
	select {
	case got := <-out:
		if !strings.Contains(got, "pty-ok") {
			t.Fatalf("the sandboxed command did not run under the PTY: %q", got)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the sandboxed command never produced output under the PTY")
	}
}
