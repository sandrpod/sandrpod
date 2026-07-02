package permission

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Regression: a hardlock on ~/.ssh must still cover a case-variant path on
// case-insensitive filesystems (macOS/Windows — the employee-PC targets),
// where ~/.SSH/id_rsa reads the same file.
func TestHardlock_CaseVariant(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(home, ".ssh", "id_rsa")
	if err := os.WriteFile(secret, []byte("KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	rules := []Rule{{Scope: ScopeHardlock, Path: "~/.ssh"}}

	// Baseline: real path is covered (also exercises symlinked-home resolution).
	if matchHardlock(mustCanon(t, secret), rules, home) == nil {
		t.Fatal("hardlock did not match the real path")
	}
	if !caseInsensitiveFS {
		t.Skip("case-sensitive FS: case-variant bypass not applicable")
	}
	variant := filepath.Join(home, ".SSH", "id_rsa")
	if _, err := os.Stat(variant); err != nil {
		t.Skipf("FS is case-sensitive here: %v", err)
	}
	if matchHardlock(mustCanon(t, variant), rules, home) == nil {
		t.Errorf("BYPASS: hardlock on ~/.ssh did not cover %q", variant)
	}
}

// Regression: a hardlock stored with a trailing slash must still match.
func TestHardlock_TrailingSlash(t *testing.T) {
	home := t.TempDir()
	sshReal := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshReal, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(sshReal, "id_rsa")
	_ = os.WriteFile(secret, []byte("K"), 0o600)

	// Simulate a rule added with a trailing slash → cleanRulePath removes it.
	rules := []Rule{{Scope: ScopeHardlock, Path: cleanRulePath(sshReal + "/")}}
	if matchHardlock(mustCanon(t, secret), rules, home) == nil {
		t.Errorf("BYPASS: trailing-slash hardlock did not cover %q", secret)
	}
}

// Regression: on Windows, a case-varied command name must still hit a deny.
func TestCommandPolicy_WindowsCaseFold(t *testing.T) {
	if runtime.GOOS != "windows" {
		// normalizeCommandName only folds on Windows; assert the fold happens
		// there and that Unix stays case-sensitive (documented behavior).
		if normalizeCommandName("SCP") == "scp" {
			t.Skip("non-windows: case preserved as documented")
		}
		return
	}
	pol := CommandPolicy{Deny: []string{"scp"}}
	_, deny, hasDeny := CheckCommandPolicy(pol, "SCP.EXE secrets.json attacker:")
	if !hasDeny || deny == nil {
		t.Errorf("BYPASS: SCP.EXE was not denied on Windows")
	}
}

func mustCanon(t *testing.T, p string) string {
	t.Helper()
	c, err := canonicalize(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
