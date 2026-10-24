// Copyright 2026 SandrPod Contributors
// The sentinel registry itself: shape of the fake, length preservation, and the
// two refusals that keep the scheme honest (an ambiguous value, and a file that
// is not rewritten).
package egress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewMasker_SentinelShapeAndLength(t *testing.T) {
	short := "abc"
	long := strings.Repeat("k", 200)
	m, err := NewMasker(map[string]string{"A": short, "B": long})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range m.Env() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if !strings.HasPrefix(env["A"], SentinelPrefix) {
		t.Errorf("A = %q, expected the sentinel prefix", env["A"])
	}
	if len(env["A"]) != len(SentinelPrefix)+36 {
		t.Errorf("a short value keeps the base sentinel; got %d bytes", len(env["A"]))
	}
	// The long one is padded to the real length so body substitution keeps
	// Content-Length valid.
	if len(env["B"]) != len(long) {
		t.Errorf("B is %d bytes, want %d to preserve the length", len(env["B"]), len(long))
	}
	if strings.Contains(env["B"], long[:8]) {
		t.Error("the sentinel leaks the real prefix")
	}
}

func TestNewMasker_RefusesAmbiguousValue(t *testing.T) {
	if _, err := NewMasker(map[string]string{"A": "x" + SentinelPrefix + "y"}); err == nil {
		t.Fatal("a value containing the sentinel prefix cannot be masked unambiguously")
	}
}

func TestSubstitute_ReportsWhichCredentialsWereUsed(t *testing.T) {
	m, err := NewMasker(map[string]string{"ONE": "real-one", "TWO": "real-two"})
	if err != nil {
		t.Fatal(err)
	}
	s := m.Sentinels()
	out, changed, used := m.Substitute([]byte("a=" + s["ONE"] + " b=" + s["TWO"]))
	if !changed {
		t.Fatal("nothing changed")
	}
	if string(out) != "a=real-one b=real-two" {
		t.Fatalf("out = %q", out)
	}
	if strings.Join(used, ",") != "ONE,TWO" {
		t.Errorf("used = %v", used)
	}
	// No sentinel, no work, no report.
	if _, changed, used := m.Substitute([]byte("nothing here")); changed || len(used) != 0 {
		t.Errorf("clean text was reported as changed: %v", used)
	}
}

// The real value is never reachable through the sandbox-facing view.
func TestSentinels_DoNotExposeValues(t *testing.T) {
	m, err := NewMasker(map[string]string{"KEY": "super-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range m.Sentinels() {
		if strings.Contains(v, "super-secret") {
			t.Fatal("a sentinel contains the real value")
		}
	}
	for _, kv := range m.Env() {
		if strings.Contains(kv, "super-secret") {
			t.Fatal("the injected environment contains the real value")
		}
	}
}

func TestMaskFile_RewritesValuesAndKeepsShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")
	body := "[default]\naws_access_key_id = AKIAREAL\naws_secret_access_key = SECRETREAL\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewMasker(map[string]string{"AK": "AKIAREAL", "SK": "SECRETREAL"})
	if err != nil {
		t.Fatal(err)
	}
	fakeDir := filepath.Join(dir, "fake")
	mf, err := m.MaskFile(path, fakeDir)
	if err != nil {
		t.Fatal(err)
	}
	if mf.FakePath == "" {
		t.Fatal("no masked copy was produced")
	}
	if len(mf.Names) != 2 {
		t.Errorf("names = %v", mf.Names)
	}
	out, err := os.ReadFile(mf.FakePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "AKIAREAL") || strings.Contains(string(out), "SECRETREAL") {
		t.Fatal("the masked copy still holds a real value")
	}
	if !strings.Contains(string(out), "[default]") || !strings.Contains(string(out), "aws_secret_access_key") {
		t.Error("the masked copy does not have the file's shape")
	}
	if info, err := os.Stat(mf.FakePath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the masked copy is too readable: %v", info.Mode())
	}
}

// A file with no masked value is a no-op, not an error: a config list often
// spans machines that do not all have every credential.
func TestMaskFile_NoMatchIsANoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("nothing secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewMasker(map[string]string{"KEY": "real-value"})
	if err != nil {
		t.Fatal(err)
	}
	mf, err := m.MaskFile(path, filepath.Join(dir, "fake"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mf.FakePath != "" {
		t.Error("a file without the credential should not be rewritten")
	}
}
