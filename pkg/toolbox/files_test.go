// Copyright 2024 SandrPod
// Unit tests for Executor file operations

package toolbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestExecutor returns an Executor whose workDir is a fresh temp directory
// that is automatically cleaned up when the test finishes.
func newTestExecutor(t *testing.T) *Executor {
	t.Helper()
	dir := t.TempDir()
	return &Executor{
		maxRun:  10,
		workDir: dir,
	}
}

// ---------- GetProjectDir / GetWorkDir ----------

func TestExecutor_GetProjectDir(t *testing.T) {
	e := newTestExecutor(t)
	if got := e.GetProjectDir(); got != e.workDir {
		t.Errorf("GetProjectDir() = %q, want %q", got, e.workDir)
	}
}

func TestExecutor_GetWorkDir(t *testing.T) {
	e := newTestExecutor(t)
	if got := e.GetWorkDir(); got != e.workDir {
		t.Errorf("GetWorkDir() = %q, want %q", got, e.workDir)
	}
}

// TestExecutor_GetUserHomeDir_FallbackNonEmpty checks that GetUserHomeDir
// always returns a non-empty, writable path. The old implementation hard-
// coded "/root" as the fallback which was nonsensical on Windows and on
// most macOS / non-root Linux hosts. The new implementation falls through
// os.UserHomeDir → $HOME → $USERPROFILE → os.TempDir, which guarantees
// SOMETHING usable on every platform but doesn't promise a specific
// directory name.
func TestExecutor_GetUserHomeDir_FallbackNonEmpty(t *testing.T) {
	e := newTestExecutor(t)
	got := e.GetUserHomeDir()
	if got == "" {
		t.Fatalf("GetUserHomeDir() returned empty string")
	}
	// Should at least be an existing directory (UserHomeDir, $HOME, or
	// TempDir all satisfy this).
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Errorf("GetUserHomeDir() = %q is not an existing directory (err=%v)", got, err)
	}
}

func TestExecutor_GetUserHomeDir_UsesEnv(t *testing.T) {
	e := newTestExecutor(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if got := e.GetUserHomeDir(); got != dir {
		t.Errorf("GetUserHomeDir() = %q, want %q", got, dir)
	}
}

// ---------- ListFiles ----------

func TestExecutor_ListFiles_Empty(t *testing.T) {
	e := newTestExecutor(t)
	files, err := e.ListFiles(context.Background(), e.workDir)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 entries, got %d", len(files))
	}
}

func TestExecutor_ListFiles_WithFiles(t *testing.T) {
	e := newTestExecutor(t)

	// Create a file and a sub-directory.
	if err := os.WriteFile(filepath.Join(e.workDir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.workDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	files, err := e.ListFiles(context.Background(), e.workDir)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(files))
	}

	// Verify the file entry.
	found := false
	for _, f := range files {
		if f.Name == "a.txt" {
			found = true
			if f.IsDir {
				t.Error("a.txt should not be a directory")
			}
			if f.Size != 5 {
				t.Errorf("a.txt size = %d, want 5", f.Size)
			}
		}
	}
	if !found {
		t.Error("a.txt not found in listing")
	}
}

func TestExecutor_ListFiles_EmptyPath_UsesWorkDir(t *testing.T) {
	e := newTestExecutor(t)
	if err := os.WriteFile(filepath.Join(e.workDir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := e.ListFiles(context.Background(), "")
	if err != nil {
		t.Fatalf("ListFiles(empty): %v", err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 file, got %d", len(files))
	}
}

// ---------- DeleteFile ----------

func TestExecutor_DeleteFile_RemovesFile(t *testing.T) {
	e := newTestExecutor(t)
	p := filepath.Join(e.workDir, "del.txt")
	if err := os.WriteFile(p, []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteFile(context.Background(), p); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestExecutor_DeleteFile_EmptyPath_ReturnsError(t *testing.T) {
	e := newTestExecutor(t)
	if err := e.DeleteFile(context.Background(), ""); err == nil {
		t.Error("DeleteFile('') should return error")
	}
}

func TestExecutor_DeleteFile_RemovesDirectory(t *testing.T) {
	e := newTestExecutor(t)
	dir := filepath.Join(e.workDir, "mydir")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteFile(context.Background(), dir); err != nil {
		t.Fatalf("DeleteFile(dir): %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("directory should have been deleted")
	}
}

// ---------- MoveFile ----------

func TestExecutor_MoveFile_RenamesFile(t *testing.T) {
	e := newTestExecutor(t)
	src := filepath.Join(e.workDir, "src.txt")
	dst := filepath.Join(e.workDir, "dst.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.MoveFile(context.Background(), src, dst); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("src should no longer exist")
	}
	content, err := os.ReadFile(dst)
	if err != nil || string(content) != "data" {
		t.Errorf("dst content: %q, want %q", string(content), "data")
	}
}

func TestExecutor_MoveFile_EmptyPaths_ReturnsError(t *testing.T) {
	e := newTestExecutor(t)
	if err := e.MoveFile(context.Background(), "", filepath.Join(e.workDir, "dst")); err == nil {
		t.Error("MoveFile with empty src should error")
	}
	if err := e.MoveFile(context.Background(), filepath.Join(e.workDir, "src"), ""); err == nil {
		t.Error("MoveFile with empty dst should error")
	}
}

// ---------- CreateFolder ----------

func TestExecutor_CreateFolder_CreatesDirectory(t *testing.T) {
	e := newTestExecutor(t)
	dir := filepath.Join(e.workDir, "newdir", "nested")
	if err := e.CreateFolder(context.Background(), dir); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Errorf("directory not created: %v", err)
	}
}

func TestExecutor_CreateFolder_EmptyPath_ReturnsError(t *testing.T) {
	e := newTestExecutor(t)
	if err := e.CreateFolder(context.Background(), ""); err == nil {
		t.Error("CreateFolder('') should return error")
	}
}

// ---------- GetFileInfo ----------

func TestExecutor_GetFileInfo_File(t *testing.T) {
	e := newTestExecutor(t)
	p := filepath.Join(e.workDir, "info.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := e.GetFileInfo(context.Background(), p)
	if err != nil {
		t.Fatalf("GetFileInfo: %v", err)
	}
	if info.Name != "info.txt" {
		t.Errorf("Name = %q, want info.txt", info.Name)
	}
	if info.IsDir {
		t.Error("should not be IsDir")
	}
	if info.Size != 5 {
		t.Errorf("Size = %d, want 5", info.Size)
	}
}

func TestExecutor_GetFileInfo_NotExist_ReturnsError(t *testing.T) {
	e := newTestExecutor(t)
	if _, err := e.GetFileInfo(context.Background(), filepath.Join(e.workDir, "no-such")); err == nil {
		t.Error("GetFileInfo(missing) should return error")
	}
}

// ---------- DownloadFile ----------

func TestExecutor_DownloadFile_ReturnsContent(t *testing.T) {
	e := newTestExecutor(t)
	p := filepath.Join(e.workDir, "dl.txt")
	want := []byte("file content")
	if err := os.WriteFile(p, want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := e.DownloadFile(context.Background(), p)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("content = %q, want %q", string(got), string(want))
	}
}

func TestExecutor_DownloadFile_EmptyPath_ReturnsError(t *testing.T) {
	e := newTestExecutor(t)
	if _, err := e.DownloadFile(context.Background(), ""); err == nil {
		t.Error("DownloadFile('') should return error")
	}
}

// ---------- SearchFiles ----------

func TestExecutor_SearchFiles_FindsMatch(t *testing.T) {
	e := newTestExecutor(t)
	if err := os.WriteFile(filepath.Join(e.workDir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := e.SearchFiles(context.Background(), e.workDir, "*.go")
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(result.Files) == 0 {
		t.Error("expected at least one match for *.go")
	}
}

func TestExecutor_SearchFiles_NoMatch(t *testing.T) {
	e := newTestExecutor(t)
	result, err := e.SearchFiles(context.Background(), e.workDir, "*.nonexistent")
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(result.Files) != 0 {
		t.Errorf("expected 0 matches, got %d", len(result.Files))
	}
}

// ---------- FindInFiles ----------

func TestExecutor_FindInFiles_FindsPattern(t *testing.T) {
	e := newTestExecutor(t)
	p := filepath.Join(e.workDir, "data.txt")
	if err := os.WriteFile(p, []byte("hello world\nfoo bar\nhello again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matches, err := e.FindInFiles(context.Background(), e.workDir, "hello")
	if err != nil {
		t.Fatalf("FindInFiles: %v", err)
	}
	if len(matches) == 0 {
		t.Error("expected matches for 'hello'")
	}
	for _, m := range matches {
		if !strings.Contains(m.Content, "hello") {
			t.Errorf("match content %q does not contain 'hello'", m.Content)
		}
	}
}

// ---------- ReplaceInFiles ----------

func TestExecutor_ReplaceInFiles_ReplacesContent(t *testing.T) {
	e := newTestExecutor(t)
	p := filepath.Join(e.workDir, "replace.txt")
	if err := os.WriteFile(p, []byte("foo bar foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := e.ReplaceInFiles(context.Background(), []string{p}, "foo", "baz")
	if err != nil {
		t.Fatalf("ReplaceInFiles: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least one replace result")
	}
	content, _ := os.ReadFile(p)
	if strings.Contains(string(content), "foo") {
		t.Errorf("file still contains 'foo': %q", string(content))
	}
}

// ---------- SetFilePermissions ----------

func TestExecutor_SetFilePermissions_ChangesMode(t *testing.T) {
	e := newTestExecutor(t)
	p := filepath.Join(e.workDir, "perm.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.SetFilePermissions(context.Background(), p, "", "", 0o600); err != nil {
		t.Fatalf("SetFilePermissions: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want %o", info.Mode().Perm(), 0o600)
	}
}
