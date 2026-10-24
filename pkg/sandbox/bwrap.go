// Copyright 2026 SandrPod Contributors
// Linux: bubblewrap.
//
// The shape is the one every container runtime uses: a read-only root, a small
// writable set, and the sensitive paths masked.
//
//	bwrap --ro-bind / /                      # the whole filesystem, read-only
//	      --bind <work_dir> <work_dir>       # the one tree that is writable
//	      --bind /tmp /tmp --bind $TMPDIR …  # temp dirs tools expect
//	      --ro-bind /dev/null <hardlocked file>
//	      --tmpfs <hardlocked directory>
//	      --dev /dev --proc /proc --die-with-parent --unshare-pid --new-session
//
// Hardlocks are masked rather than denied: a bind mount over the path makes it
// absent (a file reads as /dev/null, a directory reads as empty), which is the
// only thing a path-based mount can express, and it behaves the same for every
// process in the tree, whoever it is.
//
// `--unshare-net` is available (Policy.NetworkNone) but off by default: the
// egress proxy listens on the host's loopback, so a sandbox without a network
// namespace bridge cannot reach the proxy either. Taking the network away
// needs the relay that sandbox-runtime builds with a unix socket plus socat,
// and that is a separate change.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Bwrap wraps commands in bubblewrap.
type Bwrap struct {
	// Path is the bwrap binary; "bwrap" resolves through PATH.
	Path string
}

// Name reports the backend.
func (b *Bwrap) Name() string { return "bwrap" }

// Available checks that bwrap exists and can create a user namespace here.
func (b *Bwrap) Available() error {
	path, err := exec.LookPath(b.binary())
	if err != nil {
		return fmt.Errorf("bubblewrap not found (install `bwrap` to enable the OS sandbox): %w", err)
	}
	// A sandbox that cannot start is worse than none: probe with the cheapest
	// possible invocation. This catches a kernel without unprivileged user
	// namespaces, and a bwrap that is not setuid where it needs to be.
	probe := exec.Command(path, "--ro-bind", "/", "/", "--unshare-pid", "--", "/bin/true")
	if out, err := probe.CombinedOutput(); err != nil {
		return fmt.Errorf("bubblewrap cannot start here (%v): %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *Bwrap) binary() string {
	if b.Path != "" {
		return b.Path
	}
	return "bwrap"
}

// Command rewrites path/args to run under bwrap.
func (b *Bwrap) Command(policy Policy, path string, args []string) (string, []string, error) {
	bwrapArgs, err := BuildBwrapArgs(policy)
	if err != nil {
		return "", nil, err
	}
	full := append(bwrapArgs, "--", path)
	full = append(full, args...)
	return b.binary(), full, nil
}

// BuildBwrapArgs renders the mount plan for a policy. Exported and pure so the
// plan can be tested — and read in a log — without a kernel.
func BuildBwrapArgs(policy Policy) ([]string, error) {
	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--die-with-parent",
		"--unshare-pid",
		"--new-session",
	}
	if policy.NetworkNone {
		args = append(args, "--unshare-net")
	}

	// Writable trees: the work_dir, the temp dirs every toolchain assumes.
	writable := append([]string{policy.WorkDir}, policy.WriteAllow...)
	writable = append(writable, "/tmp", os.TempDir())
	seen := map[string]struct{}{}
	for _, dir := range expanded(writable) {
		if dir == "/" || dir == "" {
			continue // never make the root writable
		}
		if _, dup := seen[dir]; dup {
			continue
		}
		seen[dir] = struct{}{}
		args = append(args, "--bind", dir, dir)
	}

	// Masked paths, deepest first so a parent mount cannot hide a child that
	// still needs its own mask.
	denied := expanded(policy.ReadDeny)
	sort.Sort(sort.Reverse(sort.StringSlice(denied)))
	for _, p := range denied {
		if p == "/" {
			continue
		}
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			// An empty directory: unreadable contents, and writes into it are
			// lost when the command exits rather than reaching the host.
			args = append(args, "--tmpfs", p)
			continue
		}
		// /dev/null reads as empty and rejects writes with ENOSPC/EACCES.
		args = append(args, "--ro-bind", "/dev/null", p)
	}
	return args, nil
}
