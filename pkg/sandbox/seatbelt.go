// Copyright 2026 SandrPod Contributors
// macOS: Seatbelt, via sandbox-exec.
//
// The profile says the same thing the Linux one does — everything readable but
// the hard-locked paths, the work_dir writable, the system rest of the disk
// read-only — expressed in SBPL, where the rules are applied in order and the
// last match wins:
//
//	(deny default)
//	(allow process*) (allow signal (target same-sandbox)) …   ; run at all
//	(allow file-read*)                                        ; reads, …
//	(deny  file-read* (subpath "<hardlock>"))                 ; … except these
//	(allow file-write* (subpath "<work_dir>") (subpath "/tmp") (subpath "<tmp>"))
//
// Two things to know before trusting it:
//
//   - This is the one part of the feature that could not be exercised where it
//     was written: sandbox-exec refuses to nest (`sandbox_apply: Operation not
//     permitted`) inside an already-sandboxed process, which is what a
//     developer shell under MDM turns out to be. The profile is therefore
//     gated behind an explicit `-os-sandbox=seatbelt` and a startup probe that
//     actually runs a trivial command under it; if the probe fails the agent
//     says so and does not pretend.
//   - `(deny default)` is a blunt instrument: a profile that misses one of the
//     services a toolchain expects makes every command fail in a way that looks
//     like a broken install. The allow list below is the conservative core
//     (process, signals, metadata, reads, writes to the work tree, sysctl and
//     mach lookups); widen it with real workloads, not guesses.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Seatbelt wraps commands in sandbox-exec.
type Seatbelt struct {
	// Path is the sandbox-exec binary.
	Path string
}

// Name reports the backend.
func (s *Seatbelt) Name() string { return "seatbelt" }

func (s *Seatbelt) binary() string {
	if s.Path != "" {
		return s.Path
	}
	return "/usr/bin/sandbox-exec"
}

// Available checks that sandbox-exec exists and can install a profile here.
func (s *Seatbelt) Available() error {
	path := s.binary()
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("sandbox-exec not available (%v)", err)
	}
	// The probe is the point: on a machine where Seatbelt is already in force
	// for this process (an MDM-managed shell, an existing agent sandbox),
	// sandbox_apply fails and we must not wrap commands in something that
	// cannot start.
	probe := exec.Command(path, "-p", "(version 1)(allow default)", "/usr/bin/true")
	if out, err := probe.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox-exec cannot apply a profile here (%v): %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Command rewrites path/args to run under sandbox-exec with the policy.
func (s *Seatbelt) Command(policy Policy, path string, args []string) (string, []string, error) {
	profile, err := BuildSeatbeltProfile(policy)
	if err != nil {
		return "", nil, err
	}
	full := append([]string{"-p", profile, "--", path}, args...)
	return s.binary(), full, nil
}

// BuildSeatbeltProfile renders the SBPL text for a policy. Pure, so the
// profile can be inspected (and tested) on any machine.
func BuildSeatbeltProfile(policy Policy) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")

	// Run at all: a process that cannot fork or signal its own children is not
	// a sandbox, it is a crash.
	b.WriteString("(allow process*)\n")
	b.WriteString("(allow signal (target same-sandbox))\n")
	b.WriteString("(allow file-read-metadata)\n")
	b.WriteString("(allow sysctl-read)\n")
	// Mach lookups are how macOS processes reach launchd and friends; without a
	// broad allow nothing starts. The hardlock set is about files, not IPC.
	b.WriteString("(allow mach-lookup)\n")
	b.WriteString("(allow ipc-posix-shm)\n")

	// Network: the proxy is the policy, unless the caller asked for none.
	if !policy.NetworkNone {
		b.WriteString("(allow network-outbound)\n")
	}

	// Reads: everything, then the hard-locked paths taken back. A later rule
	// wins in SBPL, which is why the denies come after the global allow.
	b.WriteString("(allow file-read*)\n")
	deny := expanded(policy.ReadDeny)
	for _, p := range deny {
		if p == "/" {
			continue
		}
		b.WriteString(fmt.Sprintf("(deny file-read* (subpath %s))\n", sbplString(p)))
	}

	// Writes: the work tree and the temp dirs only. With (deny default) the
	// rest of the disk is already read-only, so no deny pass is needed.
	writable := append([]string{policy.WorkDir}, policy.WriteAllow...)
	writable = append(writable, "/tmp", "/private/tmp", os.TempDir(), "/dev")
	var rules []string
	for _, dir := range expanded(writable) {
		if dir == "/" || dir == "" {
			continue
		}
		rules = append(rules, fmt.Sprintf("(subpath %s)", sbplString(dir)))
	}
	if len(rules) > 0 {
		b.WriteString("(allow file-write* " + strings.Join(rules, " ") + ")\n")
	}
	// Mandatory-deny paths inside the writable tree: readable (the allow of
	// file-read* stands), not writable. Later rules win, so these come last.
	for _, p := range expanded(policy.WriteDeny) {
		b.WriteString(fmt.Sprintf("(deny file-write* (subpath %s))\n", sbplString(p)))
	}
	return b.String(), nil
}

// sbplString quotes a path for an SBPL string literal.
func sbplString(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(filepath.Clean(p)) + `"`
}
