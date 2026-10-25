package permission

import (
	"os"
	"path/filepath"
	"testing"
)

// The walk must shade the git directory's config, and nothing that merely
// shares its prefix.
func TestMandatoryDenyPaths_PrefixIsNotGits(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{".github/config", ".git/config", ".github/workflows/ci.yml"} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := MandatoryDenyPaths(root, 3)
	for _, p := range got {
		if filepath.ToSlash(p) == filepath.ToSlash(filepath.Join(root, ".github/config")) {
			t.Fatalf(".github/config was shaded as a git config: %v", got)
		}
	}
	found := false
	for _, p := range got {
		if filepath.ToSlash(p) == filepath.ToSlash(filepath.Join(root, ".git/config")) {
			found = true
		}
	}
	if !found {
		t.Errorf(".git/config missing from the walk: %v", got)
	}
}
