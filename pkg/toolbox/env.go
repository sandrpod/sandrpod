// Copyright 2026 SandrPod
// Child-environment plumbing for everything the toolbox spawns.
//
// A sandboxed process only reaches the internet through the egress proxy if
// it is told where the proxy is, and the only reliable place to tell it is
// the environment it starts with. Every spawn site — one-shot Execute, a
// persistent session shell, a PTY, the code interpreter kernel, procmgr
// starts — therefore builds its environment from ChildEnv(), never from
// os.Environ() directly, so a single install (SetChildEnv) covers all of
// them.
//
// The toolbox's own outbound traffic (tunnel, audit upload) is deliberately
// NOT covered: it uses the agent's environment, so filtering the sandbox can
// never break the agent's own connection to the control plane.
package toolbox

import (
	"os"
	"strings"
	"sync"
)

var (
	childEnvMu    sync.RWMutex
	childEnvExtra []string
)

// SetChildEnv installs the environment variables every sandboxed process
// inherits. Passing nil clears them. Variables already present in the agent's
// environment are replaced, not duplicated: getenv(3) returns the first match
// in the array, so an appended duplicate would silently lose.
func SetChildEnv(extra []string) {
	childEnvMu.Lock()
	defer childEnvMu.Unlock()
	childEnvExtra = append([]string(nil), extra...)
}

// ChildEnv returns the environment for a sandboxed process: the agent's own
// environment with SetChildEnv's variables applied on top.
func ChildEnv() []string {
	childEnvMu.RLock()
	extra := childEnvExtra
	childEnvMu.RUnlock()
	return mergeEnvList(os.Environ(), extra)
}

// mergeEnvList overlays extra on base, replacing any base entry with the same
// key. It keeps the base order so the result is stable and diffable in a log.
func mergeEnvList(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	override := make(map[string]string, len(extra))
	for _, kv := range extra {
		if i := strings.IndexByte(kv, '='); i > 0 {
			override[kv[:i]] = kv[i+1:]
		}
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			key = kv[:i]
		}
		if _, replaced := override[key]; replaced {
			continue
		}
		out = append(out, kv)
	}
	// Iterate extra in its own order so PATH-like overrides stay readable.
	for _, kv := range extra {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out = append(out, kv)
		}
	}
	return out
}
