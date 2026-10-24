// Copyright 2026 SandrPod Contributors
// The bwrap plan, executed for real.
//
// Unit tests can prove the argv has the right shape; only running it proves the
// shape does what it says. This test is skipped where bubblewrap is not usable
// (no binary, no unprivileged user namespaces) — the developer container in
// CI runs it with `--privileged`:
//
//	GOOS=linux GOARCH=arm64 go test -c ./pkg/sandbox -o /tmp/sandbox.test
//	docker run --rm --privileged -v /tmp:/mnt alpine:3.20 \
//	  sh -c 'apk add -q bubblewrap && /mnt/sandbox.test -test.v -test.run Bwrap'
package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBwrap_RunsSandboxed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	w := &Bwrap{Path: "bwrap"}
	if err := w.Available(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}

	// Deliberately not t.TempDir(): the plan makes the temp dirs writable, so
	// "write outside the work_dir" has to be tested somewhere else. /var/tmp is
	// ordinary disk space, not a temp dir this package allows.
	dir, err := os.MkdirTemp("/var/tmp", "sandrpod-sandbox-")
	if err != nil {
		t.Skipf("cannot create a scratch dir outside the temp tree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	work := filepath.Join(dir, "work")
	home := filepath.Join(dir, "home")
	for _, d := range []string{work, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(home, ".ssh-key")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(work, "input.txt")
	if err := os.WriteFile(allowed, []byte("VISIBLE"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(script string) (string, error) {
		path, args, err := w.Command(Policy{WorkDir: work, ReadDeny: []string{secret, home}}, "/bin/sh", []string{"-c", script})
		if err != nil {
			t.Fatalf("Command: %v", err)
		}
		out, err := exec.Command(path, args...).CombinedOutput()
		return string(out), err
	}

	// A normal file under the work_dir is readable.
	out, err := run("cat " + allowed)
	if err != nil {
		t.Fatalf("reading an allowed file failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "VISIBLE" {
		t.Errorf("allowed file read %q", out)
	}

	// The hard-locked file is not, and neither is its directory.
	out, err = run("cat " + secret)
	if err == nil {
		t.Fatalf("the hard-locked file was readable inside the sandbox: %q", out)
	}
	if strings.Contains(out, "TOPSECRET") {
		t.Fatalf("the hard-locked file leaked: %q", out)
	}
	if _, err := run("ls " + home + " | grep -q ."); err == nil {
		t.Error("the hard-locked directory still lists entries")
	}

	// Writing inside the work_dir works.
	if out, err := run("echo written > " + filepath.Join(work, "out.txt") + " && cat " + filepath.Join(work, "out.txt")); err != nil {
		t.Errorf("writing inside work_dir failed: %v\n%s", err, out)
	}

	// Writing anywhere else does not reach the host. Two shapes, because they
	// fail differently and both matter:
	//   - a masked directory is a tmpfs, so the write "succeeds" inside it and
	//     vanishes when the command exits;
	//   - an ordinary path outside the work tree is read-only, so the write
	//     fails outright.
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := run("echo nope > " + filepath.Join(home, "escape.txt")); err == nil {
		if _, statErr := os.Stat(filepath.Join(home, "escape.txt")); statErr == nil {
			t.Error("a write into a hard-locked directory reached the host")
		}
	}
	if _, err := run("echo nope > " + filepath.Join(other, "escape.txt")); err == nil {
		t.Error("a write outside the work_dir succeeded")
	}
	if _, err := os.Stat(filepath.Join(other, "escape.txt")); err == nil {
		t.Error("a write outside the work_dir reached the host")
	}

	// A file outside the work_dir is still readable (read-only root), which is
	// the model: reads are open, hardlocks are not, writes are scoped.
	if out, err := run("cat /etc/hostname"); err != nil {
		t.Errorf("reading a system file failed: %v\n%s", err, out)
	}
}

// Removing the network is available but off by default; this pins that the
// flag actually takes the namespace away when asked.
func TestBwrap_NetworkNone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	w := &Bwrap{Path: "bwrap"}
	if err := w.Available(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}
	path, args, err := w.Command(Policy{WorkDir: t.TempDir(), NetworkNone: true},
		"/bin/sh", []string{"-c", "ip link 2>/dev/null | grep -q 'state UP' && exit 1 || exit 0"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if out, err := exec.Command(path, args...).CombinedOutput(); err != nil {
		t.Errorf("expected no interfaces up inside an unnamed network namespace: %v\n%s", err, out)
	}
}
