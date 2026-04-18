// Copyright 2024 SandrPod
// 代码执行器

package toolbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Executor 代码执行器
type Executor struct {
	mu      sync.RWMutex
	running int
	maxRun  int // 最大并发数
}

// NewExecutor 创建执行器
func NewExecutor() *Executor {
	return &Executor{
		maxRun: 10, // 默认最大并发
	}
}

// Execute 执行代码
func (e *Executor) Execute(ctx context.Context, language, code string) (*ProcessResult, error) {
	return e.ExecuteStream(ctx, language, code, nil)
}

// StreamCallback 流式输出回调
type StreamCallback func(event string, data string)

// ExecuteStream 执行代码并流式输出
func (e *Executor) ExecuteStream(ctx context.Context, language, code string, callback StreamCallback) (*ProcessResult, error) {
	e.mu.Lock()
	if e.running >= e.maxRun {
		e.mu.Unlock()
		return nil, fmt.Errorf("too many concurrent executions")
	}
	e.running++
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.running--
		e.mu.Unlock()
	}()

	start := time.Now()

	var cmd *exec.Cmd
	switch strings.ToLower(language) {
	case "python", "python3":
		cmd = exec.Command(nativePython(), "-c", code)
	case "node", "nodejs":
		cmd = exec.Command("node", "-e", code)
	case "bash", "sh", "shell":
		// On Windows, use PowerShell; on Unix, use /bin/bash.
		shell := nativeShell()
		args := append(nativeShellRunArgs(), prepareExecuteCode(code))
		cmd = exec.Command(shell, args...)
	case "powershell", "pwsh":
		cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", code)
	case "go":
		cmd = exec.Command("go", "run", "-")
		cmd.Stdin = strings.NewReader(code)
	case "ruby":
		cmd = exec.Command("ruby", "-e", code)
	case "perl":
		cmd = exec.Command("perl", "-e", code)
	case "php":
		cmd = exec.Command("php", "-r", code)
	default:
		return nil, fmt.Errorf("unsupported language: %s", language)
	}

	// 设置工作目录和环境变量
	cmd.Dir = defaultWorkDir()
	// 继承父进程的环境变量��确保 PATH 可以找到 python3/node 等命令
	cmd.Env = os.Environ()
	// 进程组隔离（Unix only；Windows 为 no-op）
	setSysProcAttr(cmd)

	// 使用管道进行流式输出
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	// 设置超时
	if ctx == nil {
		ctx = context.Background()
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	// 并发读取 stdout 和 stderr
	var wg sync.WaitGroup
	var stdout, stderr bytes.Buffer

	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(&stdout, stdoutPipe) //nolint:errcheck
	}()
	go func() {
		defer wg.Done()
		io.Copy(&stderr, stderrPipe) //nolint:errcheck
	}()

	// 等待完成或超时
	done := make(chan error, 1)
	go func() {
		wg.Wait()
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		killProcess(cmd)
		cmd.Wait() //nolint:errcheck
		return &ProcessResult{
			ExitCode:  124,
			Stdout:    stdout.String(),
			Stderr:    "Execution cancelled or timed out",
			StartedAt: start,
			EndedAt:   time.Now(),
		}, nil
	case err := <-done:
		ended := time.Now()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}

		// 发送最终输出
		if callback != nil {
			if stdout.Len() > 0 {
				callback("stdout", stdout.String())
			}
			if stderr.Len() > 0 {
				callback("stderr", stderr.String())
			}
		}

		return &ProcessResult{
			ExitCode:  exitCode,
			Stdout:    strings.TrimSuffix(stdout.String(), "\n"),
			Stderr:    strings.TrimSuffix(stderr.String(), "\n"),
			StartedAt: start,
			EndedAt:   ended,
		}, nil
	}
}

// HealthCheck 健康检查
func (e *Executor) HealthCheck() HealthCheckResult {
	result := HealthCheckResult{
		Docker: true,
	}

	// 检查 Python
	if err := exec.Command("python3", "--version").Run(); err != nil {
		result.Python = false
		result.Docker = false
	} else {
		result.Python = true
	}

	// 检查 Node
	if err := exec.Command("node", "--version").Run(); err != nil {
		result.Node = false
	} else {
		result.Node = true
	}

	return result
}

// HealthCheckResult 健康检查结果
type HealthCheckResult struct {
	Docker bool
	Python bool
	Node   bool
}

// Stats 获取统计信息
func (e *Executor) Stats() map[string]interface{} {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return map[string]interface{}{
		"running": e.running,
		"max_run": e.maxRun,
	}
}
