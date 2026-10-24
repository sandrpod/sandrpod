// Copyright 2026 SandrPod Contributors
// OS sandbox: the kernel floor under the proxy.
//
// The egress proxy (pkg/egress) is a filter: it covers processes that honour
// HTTP_PROXY. The permission gate is a filter too: it covers calls that go
// through the toolbox API. Neither can stop a process that calls connect(2) or
// open(2) directly — which is exactly what "run arbitrary code" means.
//
// This package wraps each spawned process in the platform's own sandbox, so
// those syscalls fail no matter which code path issues them:
//
//   - Linux: bubblewrap (bwrap), a read-only root, a writable work_dir, and
//     tmpfs or /dev/null mounted over the paths the operator hard-locked;
//   - macOS: a generated Seatbelt profile run under sandbox-exec, with the same
//     shape (deny by default, allow the work_dir, deny the hard-locked paths).
//
// The model is deliberately the same on both platforms — "read-only root,
// writable work tree, hardlocks masked" — because a policy that means one
// thing on Linux and another on macOS is a policy nobody can reason about.
//
// What this package does NOT claim: on macOS it is experimental (the profile is
// conservative and the startup probe decides whether it may be used at all),
// and on Linux it does not yet take the network away — the proxy stays a filter
// there until the sandbox gets a network namespace with a bridge to the host
// proxy (the sandbox-runtime approach: a unix socket plus a relay inside).
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Mode selects the sandbox backend.
type Mode string

const (
	// ModeOff leaves processes unwrapped.
	ModeOff Mode = "off"
	// ModeAuto picks the platform backend when it is usable, and reports why
	// not when it is not. It never fails startup over a missing backend.
	ModeAuto Mode = "auto"
	// ModeBwrap forces bubblewrap (Linux).
	ModeBwrap Mode = "bwrap"
	// ModeSeatbelt forces sandbox-exec (macOS).
	ModeSeatbelt Mode = "seatbelt"
)

// Policy is what the OS sandbox enforces for a spawned process.
//
// There is no read-allow list: the root is readable, the hard-locked paths are
// not. That mirrors the permission gate's model (reads are open, a small set is
// never readable) and keeps the profile small enough to audit.
type Policy struct {
	// WorkDir is the one tree the process may write to (besides temp dirs).
	WorkDir string
	// ReadDeny are paths that must not be readable at all: the hardlocks.
	ReadDeny []string
	// WriteAllow are extra writable paths (defaults cover /tmp, $TMPDIR and
	// the work_dir).
	WriteAllow []string
	// WriteDeny are paths inside the writable area that must stay read-only:
	// the mandatory-deny set (git hooks/config, shell startup files, MCP
	// config, IDE launch config). Unlike ReadDeny they remain readable — a
	// build may need to read .git/config — and only writes are refused.
	WriteDeny []string
	// ProxyOnlyPort, when set, is the only outbound destination the sandbox may
	// reach: 127.0.0.1:<port>, the egress proxy. macOS has no network namespace
	// to remove, so this is what narrows the network there — a process that
	// ignores HTTP_PROXY can still open a socket, but only to the proxy.
	ProxyOnlyPort int
	// NetworkNone removes the network namespace where the platform allows it.
	// Linux only, and off by default: the egress proxy lives on the host's
	// loopback, so taking the network away would also take the proxy away —
	// unless Relay says how to get back to it.
	NetworkNone bool
	// Relay is how the sandbox reaches the egress proxy once its network is
	// gone: a unix socket bind-mounted in, and a tiny relay on the sandbox's
	// loopback that forwards to it. Required when NetworkNone is set; without
	// it the sandbox would have no network at all, including no proxy.
	Relay *Relay
}

// Relay describes the in-sandbox bridge to the host-side proxy.
type Relay struct {
	// Binary is the host path of the relay executable (the agent binary
	// itself), bind-mounted read-only at BinaryInSandbox.
	Binary string
	// BinaryInSandbox is where the relay appears inside, e.g.
	// "/run/sandrpod/relay".
	BinaryInSandbox string
	// SocketDir is the host directory holding the proxy's unix socket; the
	// whole directory is bound so the socket file comes with it.
	SocketDir string
	// SocketDirInSandbox is where that directory appears inside.
	SocketDirInSandbox string
	// SocketName is the socket's file name inside SocketDir.
	SocketName string
	// Listen is the address the relay listens on inside the sandbox, e.g.
	// "127.0.0.1:3128". Proxy-aware tools are pointed here.
	Listen string
}

// Complete reports whether the relay has everything it needs.
func (r *Relay) Complete() bool {
	return r != nil && r.Binary != "" && r.BinaryInSandbox != "" &&
		r.SocketDir != "" && r.SocketDirInSandbox != "" && r.SocketName != "" && r.Listen != ""
}

// Wrapper is one platform's implementation.
type Wrapper interface {
	// Name is the backend name reported in logs.
	Name() string
	// Available reports whether the backend can actually run here.
	Available() error
	// Command rewrites a command so it starts inside the sandbox.
	Command(policy Policy, path string, args []string) (newPath string, newArgs []string, err error)
}

// Apply rewrites cmd to run inside the sandbox. After Apply, cmd.Path is the
// sandbox launcher and the original program is its argument, so exec's usual
// behaviour (PATH lookup, environment) is unchanged.
func Apply(w Wrapper, policy Policy, path string, args []string) (string, []string, error) {
	if w == nil {
		return path, args, nil
	}
	return w.Command(policy, path, args)
}

// detect returns the platform's natural backend, regardless of availability.
func detect() Wrapper {
	switch runtime.GOOS {
	case "linux":
		return &Bwrap{Path: "bwrap"}
	case "darwin":
		return &Seatbelt{Path: "/usr/bin/sandbox-exec"}
	default:
		return nil
	}
}

// Resolve turns a mode into a wrapper, or an error explaining why the requested
// one cannot be used. ModeAuto returning (nil, nil) means "not available here",
// which callers report rather than treat as a failure.
func Resolve(mode Mode, bwrapPath, sandboxExecPath string) (Wrapper, error) {
	m := mode
	if m == "" {
		m = ModeOff
	}
	var w Wrapper
	switch m {
	case ModeOff:
		return nil, nil
	case ModeAuto:
		w = detect()
		if w == nil {
			return nil, nil
		}
	case ModeBwrap:
		w = &Bwrap{Path: firstNonEmpty(bwrapPath, "bwrap")}
	case ModeSeatbelt:
		w = &Seatbelt{Path: firstNonEmpty(sandboxExecPath, "/usr/bin/sandbox-exec")}
	default:
		return nil, fmt.Errorf("unknown sandbox mode %q (expected off|auto|bwrap|seatbelt)", mode)
	}
	if err := w.Available(); err != nil {
		if m == ModeAuto {
			return nil, nil
		}
		return nil, err
	}
	return w, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// expanded turns "~/x" into an absolute path and drops entries that do not
// exist: a hardlock on a path that is not there would make bwrap or sandbox-exec
// fail to start, and an absent path is not readable in the first place.
func expanded(paths []string) []string {
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
		if !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
