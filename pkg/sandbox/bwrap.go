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
	"path/filepath"
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
//
// With NetworkNone the command is started by a shell that first puts the relay
// on the sandbox's loopback, so the proxy stays reachable in a namespace that
// has nothing else. The real argv travels through "$@" rather than being
// quoted into the script: the argument may be arbitrary code, and a quoting
// bug there would be a command-injection bug in the process launcher.
func (b *Bwrap) Command(policy Policy, path string, args []string) (string, []string, error) {
	bwrapArgs, err := BuildBwrapArgs(policy)
	if err != nil {
		return "", nil, err
	}
	if !policy.NetworkNone {
		full := append(bwrapArgs, "--", path)
		full = append(full, args...)
		return b.binary(), full, nil
	}

	r := policy.Relay
	// Start the relay, wait until it is actually listening, then run the
	// command. The wait matters: without it a command can lose the race and
	// see "connection refused" from a proxy that is about to exist, which is
	// indistinguishable from a refusal.
	// Next to the relay binary, which sits on the writable tmpfs /run — the
	// socket directory is mounted read-only and would refuse the file.
	ready := strings.TrimSuffix(filepath.Dir(r.BinaryInSandbox), "/") + "/relay.ready"
	script := fmt.Sprintf(
		`"%s" relay --listen %s --unix "%s" --ready "%s" & `+
			`i=0; while [ ! -e "%s" ]; do i=$((i+1)); [ $i -gt 500 ] && break; sleep 0.01; done; exec "$@"`,
		r.BinaryInSandbox, r.Listen,
		strings.TrimSuffix(r.SocketDirInSandbox, "/")+"/"+r.SocketName,
		ready, ready)
	full := append(bwrapArgs, "--", "/bin/sh", "-c", script, "sh", path)
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
		if !policy.Relay.Complete() {
			// Taking the network away without a way back to the proxy would
			// leave the sandbox unable to resolve or fetch anything, including
			// the proxy itself. That is a configuration bug, not a policy.
			return nil, fmt.Errorf("NetworkNone requires a Relay (the sandbox would have no way to reach the proxy)")
		}
		// The relay's mount points live under /run, which the read-only root
		// has already sealed: bwrap would have to mkdir there and cannot.
		// A private tmpfs at /run gives it somewhere to create them, and takes
		// nothing away — a sandboxed command has no business talking to the
		// host's runtime sockets anyway.
		args = append(args, "--unshare-net",
			"--tmpfs", "/run",
			// The relay binary and the proxy's socket directory, read-only.
			"--ro-bind", policy.Relay.Binary, policy.Relay.BinaryInSandbox,
			"--ro-bind", policy.Relay.SocketDir, policy.Relay.SocketDirInSandbox,
		)
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

	// Masked files: the fake is bound read-only over the real path, so the
	// sandbox reads sentinels and the real bytes are not in its namespace.
	// Mounted after the writable binds, which is what makes it win.
	for _, m := range policy.ReadSubstitute {
		if _, err := os.Stat(m.RealPath); err != nil {
			continue
		}
		if _, err := os.Stat(m.FakePath); err != nil {
			continue
		}
		args = append(args, "--ro-bind", m.FakePath, m.RealPath)
	}

	// Read-only islands inside the writable tree. Applied after the writable
	// binds above, which is what makes them win: a later mount at the same
	// path replaces the earlier one. The source and target are the same path,
	// so the content stays visible and only the write bit is gone.
	for _, p := range expanded(policy.WriteDeny) {
		args = append(args, "--ro-bind", p, p)
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
