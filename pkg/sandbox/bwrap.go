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
	// Probe with the real plan shape, not a minimal one: the flags that can
	// make bwrap exit (a mount point it cannot create, a symlink destination,
	// a capability it lacks) only fail for the plan that is actually used.
	probeArgs, err := BuildBwrapArgs(Policy{WorkDir: os.TempDir()})
	if err != nil {
		return fmt.Errorf("cannot render a sandbox plan: %w", err)
	}
	probe := exec.Command(path, append(probeArgs, "--", "/bin/true")...)
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
	relayLog := strings.TrimSuffix(filepath.Dir(r.BinaryInSandbox), "/") + "/relay.log"
	socketIn := strings.TrimSuffix(r.SocketDirInSandbox, "/") + "/" + r.SocketName
	// shq quotes a value for /bin/sh. The relay's fields are constants today,
	// but the plan is exported and a quote in one of them would otherwise end
	// the argument and start a command.
	q := func(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }
	// The relay's own output goes to a file, not to the command's stderr: a
	// "relay: dial …: connection refused" line appearing in a Python program's
	// stderr is a support ticket. If the relay never becomes ready, or died,
	// the command is not run at all — running it without a proxy would produce
	// failures indistinguishable from a policy denial.
	script := fmt.Sprintf(
		`%s relay --listen %s --unix %s --ready %s >%s 2>&1 & relay_pid=$!; `+
			`i=0; while [ ! -e %s ]; do i=$((i+1)); [ $i -gt 500 ] && break; sleep 0.01; done; `+
			`if [ ! -e %s ] || ! kill -0 "$relay_pid" 2>/dev/null; then `+
			`echo "sandrpod: the egress relay did not start (see %s)" >&2; exit 125; fi; exec "$@"`,
		q(r.BinaryInSandbox), q(r.Listen), q(socketIn), q(ready), q(relayLog),
		q(ready), q(ready), relayLog)
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
		// IPC and UTS are cheap and close two shared channels: SysV shared
		// memory / message queues, and the host's hostname.
		"--unshare-ipc",
		"--unshare-uts",
		"--new-session",
		// Drop every capability. When the agent runs as root (a system service
		// is the natural deployment) bwrap does not create a user namespace, so
		// without this the sandbox holds real root and `umount` would lift every
		// mask above. It is a no-op for an unprivileged agent.
		"--cap-drop", "ALL",
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
		// /dev/null reads as empty; the mount is what makes writes fail.
		args = append(args, "--ro-bind", "/dev/null", p)
	}

	// The proxy's socket directory is bound early (it must exist before the
	// relay starts), which means the writable /tmp bind above re-exposes it at
	// its host path: without this island a sandbox could unlink proxy.sock and
	// kill its own egress. Re-bound read-only here, after the writable binds.
	if policy.Relay.Complete() {
		args = append(args, "--ro-bind", policy.Relay.SocketDir, policy.Relay.SocketDir)
	}

	// Masked files go last: a hardlock directory that contains one of them
	// (the default seeds include ~/.aws, and the documented example masks
	// ~/.aws/credentials) would otherwise be tmpfs'd over the substitution and
	// the tool would find no file at all. A sentinel file there exposes
	// nothing, so mounting it after the mask is not a weakening.
	for _, m := range policy.ReadSubstitute {
		real, ok := resolveExisting(m.RealPath)
		if !ok {
			continue
		}
		fake, ok := resolveExisting(m.FakePath)
		if !ok {
			// Fail closed. Skipping the substitution would leave the real
			// credential readable inside the sandbox, which is exactly the
			// thing the operator asked to prevent.
			return nil, fmt.Errorf("masked file %s has no sentinel copy (%s); refusing to start a sandbox that would expose it",
				m.RealPath, m.FakePath)
		}
		args = append(args, "--ro-bind", fake, real)
	}
	return args, nil
}

// resolveExisting resolves a path the way the mount plan needs it: absolute,
// symlink-free, and existing. Symlinks matter because bwrap refuses to mount on
// a symlink destination.
func resolveExisting(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if _, err := os.Stat(abs); err != nil {
		return "", false
	}
	return abs, true
}
