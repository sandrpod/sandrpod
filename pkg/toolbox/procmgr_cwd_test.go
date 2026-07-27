// Copyright 2026 SandrPod Contributors

package toolbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStartRejectsUnusableCwd pins the error for a working directory that is
// not there. os/exec reports a missing Dir as ENOENT against the *binary*, so
// without this the caller is told "fork/exec /bin/bash: no such file or
// directory" and goes hunting for a shell that is present — an agent framework
// whose default workdir does not exist in the image hits this on its first call.
func TestStartRejectsUnusableCwd(t *testing.T) {
	m := NewProcManager()

	missing := filepath.Join(t.TempDir(), "not-created")
	_, err := m.Start(ProcStartConfig{Cmd: "/bin/sh", Args: []string{"-c", "true"}, Cwd: missing})
	if err == nil {
		t.Fatal("missing cwd: want an error, got none")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error must name the directory, got: %v", err)
	}
	if strings.Contains(err.Error(), "fork/exec") {
		t.Errorf("error still blames the binary: %v", err)
	}

	// A file where a directory is expected has to be caught too — exec would
	// otherwise fail with ENOTDIR, which is no clearer.
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ProcStartConfig{Cmd: "/bin/sh", Args: []string{"-c", "true"}, Cwd: file}); err == nil {
		t.Error("cwd is a file: want an error, got none")
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("want a 'not a directory' error, got: %v", err)
	}

	// A real directory still starts.
	if _, err := m.Start(ProcStartConfig{Cmd: "/bin/sh", Args: []string{"-c", "true"}, Cwd: t.TempDir()}); err != nil {
		t.Errorf("usable cwd: %v", err)
	}
}
