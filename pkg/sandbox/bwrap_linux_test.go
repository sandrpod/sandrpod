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

// The network-namespace half lives in netns_linux_test.go: it needs a relay to
// be useful, so it is tested through the whole chain rather than alone.

// Write denies are islands in the writable tree: readable, not writable. This
// is the mandatory-deny set — .git/hooks, .bashrc — enforced by the kernel
// rather than by the file API.
func TestBwrap_WriteDenyIslands(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	w := &Bwrap{Path: "bwrap"}
	if err := w.Available(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}

	dir, err := os.MkdirTemp("/var/tmp", "sandrpod-wd-")
	if err != nil {
		t.Skipf("no scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	work := filepath.Join(dir, "work")
	hooks := filepath.Join(work, ".git", "hooks")
	for _, d := range []string{hooks, filepath.Join(work, "src")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hook := filepath.Join(hooks, "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho original\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(work, ".bashrc")
	if err := os.WriteFile(rc, []byte("export ORIGINAL=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(work, "src", "main.go")
	if err := os.WriteFile(plain, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	policy := Policy{WorkDir: work, WriteDeny: []string{hooks, rc, filepath.Join(work, ".git", "config")}}
	run := func(script string) (string, error) {
		path, args, err := w.Command(policy, "/bin/sh", []string{"-c", script})
		if err != nil {
			t.Fatalf("Command: %v", err)
		}
		out, err := exec.Command(path, args...).CombinedOutput()
		return string(out), err
	}

	// Reading the hook still works — a build may need to.
	if out, err := run("cat " + hook); err != nil || !strings.Contains(out, "original") {
		t.Errorf("the hook should stay readable: err=%v out=%q", err, out)
	}
	// Writing it does not, and the host's copy is untouched.
	if _, err := run("echo pwned >> " + hook); err == nil {
		t.Error("writing a git hook succeeded inside the sandbox")
	}
	if b, _ := os.ReadFile(hook); strings.Contains(string(b), "pwned") {
		t.Fatal("the hook was modified on the host")
	}
	// Same for a shell startup file.
	if _, err := run("echo pwned >> " + rc); err == nil {
		t.Error("writing .bashrc succeeded inside the sandbox")
	}
	// An ordinary file in the same tree is still writable.
	if out, err := run("echo ok > " + plain + " && cat " + plain); err != nil || !strings.Contains(out, "ok") {
		t.Errorf("ordinary files must stay writable: err=%v out=%q", err, out)
	}
}

// A masked file: the sandbox reads a fake bound over the real path, so the real
// bytes are not in its namespace at all.
func TestBwrap_ReadSubstitution(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	w := &Bwrap{Path: "bwrap"}
	if err := w.Available(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}

	base, err := os.MkdirTemp("/var/tmp", "sandrpod-sub-")
	if err != nil {
		t.Skipf("no scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(work, "credentials")
	if err := os.WriteFile(realFile, []byte("aws_secret_access_key = REAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeFile := filepath.Join(base, "fake-credentials")
	if err := os.WriteFile(fakeFile, []byte("aws_secret_access_key = fake_value_xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := Policy{
		WorkDir:        work,
		ReadSubstitute: []ReadSubstitution{{RealPath: realFile, FakePath: fakeFile}},
	}
	path, args, err := w.Command(policy, "/bin/sh", []string{"-c", "cat " + realFile})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	out, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("cat: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "REAL") {
		t.Fatalf("the sandbox read the real file: %q", out)
	}
	if !strings.Contains(string(out), "fake_value_xyz") {
		t.Fatalf("the sandbox did not see the substitute: %q", out)
	}
}
