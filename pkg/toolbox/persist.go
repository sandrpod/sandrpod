package toolbox

import (
	"io"
	"os"
	"path/filepath"
)

// PersistHome links ~/.sandrpod into the workspace volume.
//
// The volume is what survives a container rebuild, and ~/.sandrpod holds state the
// user had to work for: mcp.json (which MCP servers exist) and oauth/*.json (the
// tokens they granted). Left in the container layer it survives a stop/start and a
// snapshot, but vanishes the moment the container is recreated — an image upgrade
// silently revoked every third-party authorization.
//
// No-op when there is no workspace (the agent runs on a bare host, not in a
// container) or when the link is already in place (every restart after the first).
//
// Returns whether it moved anything, so the caller can log it once.
func PersistHome(home, workspaceRoot string) (bool, error) {
	if workspaceRoot == "" {
		return false, nil
	}
	if fi, err := os.Stat(workspaceRoot); err != nil || !fi.IsDir() {
		return false, nil // not a sandbox container: a host agent has no volume
	}
	src := filepath.Join(home, ".sandrpod")
	if fi, err := os.Lstat(src); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return false, nil // already linked
	}
	dst := filepath.Join(workspaceRoot, ".home", ".sandrpod")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return false, err
	}
	// Tokens live here; the directory must not be world readable even before the
	// first copy (umask would otherwise decide).
	if err := os.Chmod(dst, 0o700); err != nil {
		return false, err
	}
	if fi, err := os.Stat(src); err == nil && fi.IsDir() {
		if err := copyTree(src, dst); err != nil {
			return false, err
		}
		if err := os.RemoveAll(src); err != nil {
			return false, err
		}
	}
	if err := os.Symlink(dst, src); err != nil {
		return false, err
	}
	return true, nil
}

// copyTree copies modes as well as contents: OAuth token files are 0600 and must
// stay that way.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}
