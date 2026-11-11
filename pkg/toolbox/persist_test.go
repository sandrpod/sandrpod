package toolbox

import (
	"os"
	"path/filepath"
	"testing"
)

// A headless sandbox keeps mcp.json and the OAuth tokens in ~/.sandrpod. If they
// stay in the container layer, recreating the container (an image upgrade) drops
// every authorization the user granted.
func TestPersistHomeMovesStateIntoTheVolume(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	src := filepath.Join(home, ".sandrpod")
	if err := os.MkdirAll(filepath.Join(src, "oauth"), 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(src, "oauth", "github.json")
	if err := os.WriteFile(token, []byte(`{"access_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "mcp.json"), []byte(`{"servers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	moved, err := PersistHome(home, ws)
	if err != nil {
		t.Fatalf("PersistHome: %v", err)
	}
	if !moved {
		t.Fatal("nothing was moved")
	}

	// The link points into the volume, and the token is readable through it with
	// its 0600 mode intact.
	fi, err := os.Lstat(src)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("~/.sandrpod is not a symlink: %v", err)
	}
	if b, err := os.ReadFile(token); err != nil || string(b) != `{"access_token":"x"}` {
		t.Fatalf("token unreadable after the move: %v %s", err, b)
	}
	tfi, err := os.Stat(filepath.Join(ws, ".home", ".sandrpod", "oauth", "github.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tfi.Mode().Perm(); got != 0o600 {
		t.Fatalf("token mode = %o, want 600 (the copy must not widen it)", got)
	}
	if dfi, err := os.Stat(filepath.Join(ws, ".home", ".sandrpod")); err != nil || dfi.Mode().Perm() != 0o700 {
		t.Fatalf("volume dir mode = %v, want 700", dfi.Mode().Perm())
	}

	// Second call is a no-op: this runs on every start.
	if moved, err := PersistHome(home, ws); err != nil || moved {
		t.Fatalf("second call moved=%v err=%v, want no-op", moved, err)
	}
}

func TestPersistHomeNoopOutsideAContainer(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".sandrpod"), 0o700); err != nil {
		t.Fatal(err)
	}
	// No workspace (agent mode) and a missing directory both leave the tree alone.
	if moved, err := PersistHome(home, ""); err != nil || moved {
		t.Fatalf("empty root: moved=%v err=%v", moved, err)
	}
	if moved, err := PersistHome(home, filepath.Join(t.TempDir(), "nope")); err != nil || moved {
		t.Fatalf("missing root: moved=%v err=%v", moved, err)
	}
	if fi, err := os.Lstat(filepath.Join(home, ".sandrpod")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the real directory was disturbed outside a container")
	}
}
