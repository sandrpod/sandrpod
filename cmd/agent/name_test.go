// Copyright 2026 SandrPod Contributors

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestResolveSandboxName_ExplicitWins(t *testing.T) {
	dir := t.TempDir()
	if got := resolveSandboxName("my-box", dir); got != "my-box" {
		t.Fatalf("name = %q, want the explicit value", got)
	}
	if _, err := os.Stat(filepath.Join(dir, agentNameFile)); !os.IsNotExist(err) {
		t.Fatal("an explicit name must not write a generated name")
	}
}

func TestResolveSandboxName_GeneratedIsStable(t *testing.T) {
	dir := t.TempDir()
	first := resolveSandboxName("", dir)
	second := resolveSandboxName("", dir)
	if first != second {
		t.Fatalf("name changed across starts: %q then %q", first, second)
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*-[0-9a-f]{4}$`).MatchString(first) {
		t.Fatalf("generated name %q is not hostname-suffix", first)
	}
	if len(first) > 53 {
		t.Fatalf("generated name %q is too long", first)
	}
	data, err := os.ReadFile(filepath.Join(dir, agentNameFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != first {
		t.Fatalf("persisted name %q, want %q", data, first)
	}
	info, err := os.Stat(filepath.Join(dir, agentNameFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("name file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestResolveSandboxName_HostnameFailureFallsBack(t *testing.T) {
	name := generatedSandboxName(func() (string, error) { return "", os.ErrNotExist })
	if !strings.HasPrefix(name, "agent-") {
		t.Fatalf("fallback name = %q, want an agent- prefix", name)
	}
}

func TestSanitizeHostLabel(t *testing.T) {
	cases := map[string]string{
		"My-Laptop.local":       "my-laptop.local",
		"MacBook Pro":           "macbook-pro",
		"/weird:host":           "weird-host",
		"123box":                "123box",
		".hidden":               "hidden",
		"":                      "",
		strings.Repeat("a", 80): strings.Repeat("a", 48),
	}
	for in, want := range cases {
		if got := sanitizeHostLabel(in); got != want {
			t.Errorf("sanitizeHostLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
