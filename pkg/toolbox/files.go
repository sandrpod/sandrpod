// Copyright 2024 SandrPod
// File operation interface.
//
// Each method takes a context.Context as its first argument so the permission
// manager (installed via Executor.SetPermissionManager) can:
//   1. Attach a per-request deadline to its consent prompt.
//   2. Read the sandbox-session id stored on the context (see WithSandboxSession).
//
// HTTP handlers in api.go must pass r.Context() through. Tests and tools that
// don't care about cancellation can pass context.Background().

package toolbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sandrpod/sandrpod/pkg/homedir"
	"github.com/sandrpod/sandrpod/pkg/permission"
)

// FileInfo describes a file or directory entry.
type FileInfo struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// GetProjectDir returns the project directory (the executor's working directory).
func (e *Executor) GetProjectDir() string {
	return e.workDir
}

// GetUserHomeDir returns the base directory under which sandrpod's data (incl.
// the personal skills dir the platform reads as <home>/.sandrpod/skills) lives.
//
// This is os.UserHomeDir() on a normal login, but a Windows service account
// (LocalSystem) resolves it under System32\config\systemprofile, which the file
// gate blocks — so pkg/homedir redirects that case to %ProgramData%. See
// pkg/homedir for the full rationale.
func (e *Executor) GetUserHomeDir() string {
	return homedir.DataHome()
}

// GetWorkDir returns the working directory (equivalent to the project directory).
func (e *Executor) GetWorkDir() string {
	return e.workDir
}

// ListFiles returns the entries in the given directory.
func (e *Executor) ListFiles(ctx context.Context, path string) ([]*FileInfo, error) {
	safe, err := e.resolveAndAuthorize(ctx, path, permission.ModeRead, "files.list")
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(safe)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	var files []*FileInfo
	for _, entry := range entries {
		info, err := entry.Info()
		size := int64(0)
		if err == nil {
			size = info.Size()
		}
		files = append(files, &FileInfo{
			Name:  entry.Name(),
			Path:  filepath.Join(safe, entry.Name()),
			IsDir: entry.IsDir(),
			Size:  size,
		})
	}
	return files, nil
}

// DeleteFile removes a file or directory (recursively).
func (e *Executor) DeleteFile(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	safe, err := e.resolveAndAuthorize(ctx, path, permission.ModeWrite, "files.delete")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(safe); err != nil {
		return fmt.Errorf("failed to delete: %w", err)
	}
	return nil
}

// MoveFile moves or renames a file or directory.
func (e *Executor) MoveFile(ctx context.Context, src, dst string) error {
	if src == "" || dst == "" {
		return fmt.Errorf("source and destination are required")
	}
	safeSrc, err := e.resolveAndAuthorize(ctx, src, permission.ModeWrite, "files.move.src")
	if err != nil {
		return err
	}
	safeDst, err := e.resolveAndAuthorize(ctx, dst, permission.ModeWrite, "files.move.dst")
	if err != nil {
		return err
	}
	if err := os.Rename(safeSrc, safeDst); err != nil {
		return fmt.Errorf("failed to move: %w", err)
	}
	return nil
}

// CreateFolder creates a directory (and any missing parents).
func (e *Executor) CreateFolder(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	safe, err := e.resolveAndAuthorize(ctx, path, permission.ModeWrite, "files.mkdir")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(safe, 0755); err != nil {
		return fmt.Errorf("failed to create folder: %w", err)
	}
	return nil
}

// GetFileInfo returns metadata for the file or directory at path.
func (e *Executor) GetFileInfo(ctx context.Context, path string) (*FileInfo, error) {
	safe, err := e.resolveAndAuthorize(ctx, path, permission.ModeRead, "files.info")
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(safe)
	if err != nil {
		return nil, fmt.Errorf("failed to stat: %w", err)
	}
	return &FileInfo{
		Name:  filepath.Base(safe),
		Path:  safe,
		IsDir: info.IsDir(),
		Size:  info.Size(),
	}, nil
}

// DownloadFile reads and returns the contents of a file.
func (e *Executor) DownloadFile(ctx context.Context, path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	safe, err := e.resolveAndAuthorize(ctx, path, permission.ModeRead, "files.download")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(safe)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	return data, nil
}

// SearchFilesResult holds the list of files matched by a glob search.
type SearchFilesResult struct {
	Files []string `json:"files"`
}

// FindMatch represents a single line match from a content search.
type FindMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

// SearchFiles finds files matching a glob pattern within a directory.
func (e *Executor) SearchFiles(ctx context.Context, path, pattern string) (*SearchFilesResult, error) {
	safe, err := e.resolveAndAuthorize(ctx, path, permission.ModeRead, "files.search")
	if err != nil {
		return nil, err
	}
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	matches, err := filepath.Glob(filepath.Join(safe, pattern))
	if err != nil {
		return nil, fmt.Errorf("failed to search: %w", err)
	}
	return &SearchFilesResult{Files: matches}, nil
}

// FindInFiles searches file contents for lines containing pattern, walking the directory tree.
func (e *Executor) FindInFiles(ctx context.Context, path, pattern string) ([]*FindMatch, error) {
	safe, err := e.resolveAndAuthorize(ctx, path, permission.ModeRead, "files.find")
	if err != nil {
		return nil, err
	}
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	// Bounds so a walk over a huge tree (or `/`) can't exhaust memory: skip
	// oversized/binary files instead of slurping them whole, and stop once
	// enough matches are collected.
	const (
		findMaxFileSize = 5 << 20 // 5 MiB per file
		findMaxMatches  = 5000
	)
	var results []*FindMatch
	err = filepath.Walk(safe, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip files that cannot be accessed
		}
		if info.IsDir() {
			return nil
		}
		if info.Size() > findMaxFileSize {
			return nil
		}

		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil
		}

		for i, line := range strings.Split(string(content), "\n") {
			if strings.Contains(line, pattern) {
				results = append(results, &FindMatch{
					File:    filePath,
					Line:    i + 1,
					Content: line,
				})
				if len(results) >= findMaxMatches {
					return filepath.SkipAll // enough — stop the walk
				}
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to find: %w", err)
	}
	return results, nil
}

// ReplaceResult reports the outcome of a text replacement operation on a single file.
type ReplaceResult struct {
	File    string `json:"file"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// ReplaceInFiles replaces all occurrences of pattern with newValue across the given files.
func (e *Executor) ReplaceInFiles(ctx context.Context, files []string, pattern, newValue string) ([]*ReplaceResult, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("files is required")
	}
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	var results []*ReplaceResult
	for _, file := range files {
		// Validate that each file path is within an allowed write location.
		safe, err := e.resolveAndAuthorize(ctx, file, permission.ModeWrite, "files.replace")
		if err != nil {
			results = append(results, &ReplaceResult{File: file, Success: false, Error: err.Error()})
			continue
		}

		content, err := os.ReadFile(safe)
		if err != nil {
			results = append(results, &ReplaceResult{File: safe, Success: false, Error: err.Error()})
			continue
		}

		newContent := strings.ReplaceAll(string(content), pattern, newValue)
		if err := os.WriteFile(safe, []byte(newContent), 0644); err != nil {
			results = append(results, &ReplaceResult{File: safe, Success: false, Error: err.Error()})
			continue
		}

		results = append(results, &ReplaceResult{File: safe, Success: true})
	}
	return results, nil
}

// SetFilePermissions sets the mode bits and optionally the owner/group of a file.
func (e *Executor) SetFilePermissions(ctx context.Context, path, owner, group string, mode os.FileMode) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	safe, err := e.resolveAndAuthorize(ctx, path, permission.ModeWrite, "files.chmod")
	if err != nil {
		return err
	}

	if err := os.Chmod(safe, mode); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	if owner != "" || group != "" {
		var uid, gid int = -1, -1
		if owner != "" {
			fmt.Sscanf(owner, "%d", &uid)
		}
		if group != "" {
			fmt.Sscanf(group, "%d", &gid)
		}
		if uid >= 0 || gid >= 0 {
			os.Chown(safe, uid, gid) //nolint:errcheck — chown may fail without root; non-fatal
		}
	}

	return nil
}
