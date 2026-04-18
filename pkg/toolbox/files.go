// Copyright 2024 SandrPod
// 文件操作接口

package toolbox

import (
	"fmt"
	"os"
	"path/filepath"
)

// FileInfo 文件信息
type FileInfo struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// GetProjectDir 获取项目目录 (当前工作目录)
func (e *Executor) GetProjectDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "/workspace"
	}
	return cwd
}

// GetUserHomeDir 获取用户 home 目录
func (e *Executor) GetUserHomeDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return home
}

// GetWorkDir 获取工作目录 (等同于项目目录)
func (e *Executor) GetWorkDir() string {
	return e.GetProjectDir()
}

// ListFiles 列出目录内容
func (e *Executor) ListFiles(path string) ([]*FileInfo, error) {
	if path == "" {
		path = e.GetProjectDir()
	}

	// 安全检查：不允许访问 / 根目录
	if path == "/" {
		return nil, fmt.Errorf("access to root denied")
	}

	entries, err := os.ReadDir(path)
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
			Path:  filepath.Join(path, entry.Name()),
			IsDir: entry.IsDir(),
			Size:  size,
		})
	}
	return files, nil
}

// DeleteFile 删除文件或目录
func (e *Executor) DeleteFile(path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}

	// 安全检查：不允许删除系统目录
	if path == "/" || path == "/app" || path == "/root" || path == "/home" {
		return fmt.Errorf("cannot delete system directory")
	}

	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("failed to delete: %w", err)
	}
	return nil
}

// MoveFile 移动或重命名文件/目录
func (e *Executor) MoveFile(src, dst string) error {
	if src == "" || dst == "" {
		return fmt.Errorf("source and destination are required")
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("failed to move: %w", err)
	}
	return nil
}

// CreateFolder 创建目录
func (e *Executor) CreateFolder(path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("failed to create folder: %w", err)
	}
	return nil
}

// GetFileInfo 获取文件信息
func (e *Executor) GetFileInfo(path string) (*FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat: %w", err)
	}
	return &FileInfo{
		Name:  filepath.Base(path),
		Path:  path,
		IsDir: info.IsDir(),
		Size:  info.Size(),
	}, nil
}

// DownloadFile 下载文件内容
func (e *Executor) DownloadFile(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	return data, nil
}

// SearchFilesResult 搜索文件结果
type SearchFilesResult struct {
	Files []string `json:"files"`
}

// FindMatch 搜索匹配结果
type FindMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

// SearchFiles 搜索文件 (glob模式)
func (e *Executor) SearchFiles(path, pattern string) (*SearchFilesResult, error) {
	if path == "" {
		path = e.GetProjectDir()
	}
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	matches, err := filepath.Glob(filepath.Join(path, pattern))
	if err != nil {
		return nil, fmt.Errorf("failed to search: %w", err)
	}
	return &SearchFilesResult{Files: matches}, nil
}

// FindInFiles 文件内容搜索
func (e *Executor) FindInFiles(path, pattern string) ([]*FindMatch, error) {
	if path == "" {
		path = e.GetProjectDir()
	}
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	var results []*FindMatch
	err := filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 跳过无法访问的文件
		}
		if info.IsDir() {
			return nil
		}

		// 读取文件内容
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil
		}

		lines := filepath.SplitList(string(content))
		for i, line := range lines {
			if contains(line, pattern) {
				results = append(results, &FindMatch{
					File:    filePath,
					Line:    i + 1,
					Content: line,
				})
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to find: %w", err)
	}
	return results, nil
}

// contains 检查字符串是否包含子串
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ReplaceResult 替换结果
type ReplaceResult struct {
	File    string `json:"file"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// ReplaceInFiles 文本替换
func (e *Executor) ReplaceInFiles(files []string, pattern, newValue string) ([]*ReplaceResult, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("files is required")
	}
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	var results []*ReplaceResult
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			results = append(results, &ReplaceResult{
				File:    file,
				Success: false,
				Error:   err.Error(),
			})
			continue
		}

		newContent := replaceAll(string(content), pattern, newValue)
		if err := os.WriteFile(file, []byte(newContent), 0644); err != nil {
			results = append(results, &ReplaceResult{
				File:    file,
				Success: false,
				Error:   err.Error(),
			})
			continue
		}

		results = append(results, &ReplaceResult{
			File:    file,
			Success: true,
		})
	}
	return results, nil
}

// replaceAll 替换所有匹配
func replaceAll(s, old, new string) string {
	result := ""
	for {
		idx := indexOf(s, old)
		if idx == -1 {
			result += s
			break
		}
		result += s[:idx] + new
		s = s[idx+len(old):]
	}
	return result
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// SetFilePermissions 设置文件权限
func (e *Executor) SetFilePermissions(path, owner, group string, mode os.FileMode) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}

	// 设置权限
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// 如果提供了 owner/group，尝试更改所有权 (需要 root 权限)
	if owner != "" || group != "" {
		// 解析 uid/gid
		var uid, gid int = -1, -1
		if owner != "" {
			// 简化处理：假设 owner 是数字 uid
			fmt.Sscanf(owner, "%d", &uid)
		}
		if group != "" {
			fmt.Sscanf(group, "%d", &gid)
		}
		if uid >= 0 || gid >= 0 {
			if err := os.Chown(path, uid, gid); err != nil {
				// 忽略 chown 错误 (可能没有权限)
			}
		}
	}

	return nil
}
