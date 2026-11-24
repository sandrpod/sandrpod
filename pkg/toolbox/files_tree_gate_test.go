package toolbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// A search authorizes its root once. Before the per-path filter, everything
// beneath the root was then read unchecked: searching an authorized ~ for
// "PRIVATE KEY" returned the lines of ~/.ssh/id_rsa even though ~/.ssh is
// hard-locked, and a symlink inside the root was followed anywhere.
func TestTreeSearchHonoursHardlocksAndSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "notes.txt"), "SECRET-MARKER visible\n")
	write(filepath.Join(root, ".ssh", "id_rsa"), "SECRET-MARKER locked\n")
	write(filepath.Join(outside, "other.txt"), "SECRET-MARKER outside\n")
	if err := os.Symlink(filepath.Join(outside, "other.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".ssh", "id_rsa"), filepath.Join(root, "key-link")); err != nil {
		t.Fatal(err)
	}

	store, err := permission.LoadStore(filepath.Join(t.TempDir(), "permissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddHardlock(filepath.Join(root, ".ssh")); err != nil {
		t.Fatal(err)
	}
	mgr, err := permission.NewManager(permission.Options{
		Store: store, Notifier: permission.NopNotifier{}, WorkDir: root, HomeDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor()
	e.SetPermissionManager(mgr)
	ctx := context.Background()

	matches, err := e.FindInFiles(ctx, root, "SECRET-MARKER")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range matches {
		got = append(got, m.Content)
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "visible") {
		t.Errorf("the ordinary file must still match, got %q", joined)
	}
	if strings.Contains(joined, "locked") {
		t.Errorf("content under a hardlock leaked through the search: %q", joined)
	}
	if strings.Contains(joined, "outside") {
		t.Errorf("a symlink out of the authorized root was followed: %q", joined)
	}

	// The glob search: matches under the hardlock, behind an outward link, or
	// reached by climbing out with "..", are all dropped.
	for _, pattern := range []string{"*", ".ssh/*", "../" + filepath.Base(outside) + "/*"} {
		res, err := e.SearchFiles(ctx, root, pattern)
		if err != nil {
			t.Fatalf("pattern %q: %v", pattern, err)
		}
		for _, f := range res.Files {
			base := filepath.Base(f)
			if base == "id_rsa" || base == "other.txt" || base == "link.txt" || base == "key-link" || base == ".ssh" {
				t.Errorf("pattern %q returned %s, which is outside what was authorized", pattern, f)
			}
		}
	}
	res, err := e.SearchFiles(ctx, root, "*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || filepath.Base(res.Files[0]) != "notes.txt" {
		t.Errorf("*.txt: got %v, want only notes.txt", res.Files)
	}
}
