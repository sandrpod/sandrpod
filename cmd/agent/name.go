// Copyright 2026 SandrPod Contributors
//
// A machine's sandbox name: unique per server, stable per machine.
//
// Sandbox names are the routing key for everything the product exposes on the
// wildcard host — the desktop (<port>-<name>.<domain>), CDP, the MCP bridge — and
// the control plane makes them unique across the whole server, not per tenant:
// two workspaces cannot both own "macbook". A bare hostname is therefore a
// collision waiting to happen (every default "localhost", every fleet image, two
// customers with the same laptop name). The generated name adds a short random
// suffix once and keeps it in the state directory, so a restart is the same
// sandbox and a second machine never takes the first one's name.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const agentNameFile = "agent-name"

// resolveSandboxName returns the explicit name when one was given, else the
// persisted generated name, else a new one (hostname + 4 hex) written to disk.
func resolveSandboxName(explicit, stateDir string) string {
	if n := strings.TrimSpace(explicit); n != "" {
		return n
	}
	path := filepath.Join(stateDir, agentNameFile)
	if b, err := os.ReadFile(path); err == nil {
		if n := strings.TrimSpace(string(b)); n != "" {
			return n
		}
	}
	name := generatedSandboxName(os.Hostname)
	if err := writeAgentName(path, name); err != nil {
		log.Printf("WARNING: could not persist the sandbox name to %s (%v); the next start will pick a different one", path, err)
	}
	log.Printf("Sandbox name: %s (generated once; stored in %s)", name, path)
	return name
}

// generatedSandboxName takes a hostname provider so the formatting is testable.
func generatedSandboxName(hostname func() (string, error)) string {
	host := ""
	if h, err := hostname(); err == nil {
		host = sanitizeHostLabel(h)
	}
	if host == "" {
		host = "agent"
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		// A broken RNG is a broken machine; the pid still gives uniqueness
		// within the fleet, which is all this suffix is for.
		pid := os.Getpid()
		suffix = []byte{byte(pid >> 24), byte(pid >> 16), byte(pid >> 8), byte(pid)}
	}
	return host + "-" + hex.EncodeToString(suffix)
}

// sanitizeHostLabel maps a hostname to the sandbox-name alphabet
// ([A-Za-z0-9._-], starting alphanumeric) and keeps it short enough to leave
// room for the suffix.
func sanitizeHostLabel(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r), r == '/', r == ':', r == '@':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 48 {
			break
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		return ""
	}
	if c := out[0]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		out = "agent-" + out
	}
	return out
}

func writeAgentName(path, name string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(name+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
