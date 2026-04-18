// Copyright 2024 SandrPod
// Session Manager - 会话核心管理逻辑

package toolbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// PollingInterval 轮询间隔
	PollingInterval = 50 * time.Millisecond
	// DefaultTimeout 默认命令超时
	DefaultTimeout = 30 * time.Second
	// DefaultSessionTTL 默认 session 存活时间 (30分钟无活动则清理)
	DefaultSessionTTL = 30 * time.Minute
	// CleanupInterval 清理检查间隔
	CleanupInterval = 5 * time.Minute
)

// Execute 执行命令
func (m *SessionManager) Execute(sessionId, cmdId, command string, async bool) (*SessionExecuteResponse, error) {
	session, ok := m.Get(sessionId)
	if !ok {
		return nil, &SessionError{Op: "execute", Err: ErrSessionNotFound}
	}

	session.mu.Lock()
	if session.Closed {
		session.mu.Unlock()
		return nil, &SessionError{Op: "execute", Err: ErrSessionClosed}
	}
	session.mu.Unlock()

	// 生成 cmdId
	if cmdId == "" {
		cmdId = uuid.NewString()
	}

	// 创建命令目录
	cmdDir := filepath.Join(session.Dir, cmdId)
	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		return nil, &SessionError{Op: "execute", Err: err}
	}

	logFile := filepath.Join(cmdDir, "output.log")
	exitFile := filepath.Join(cmdDir, "exit_code")

	sessionCommand := &SessionCommand{
		ID:       cmdId,
		Command:  command,
		LogFile:  logFile,
		ExitFile: exitFile,
		CreatedAt: time.Now(),
	}

	// 添加到 commands map 并更新活动时间
	session.mu.Lock()
	session.Commands[cmdId] = sessionCommand
	session.LastActivity = time.Now()
	session.mu.Unlock()

	// 构建命令包装脚本（通过平台函数生成 bash/PowerShell 语法）
	cmdWrapper := buildCommandWrapper(command, logFile, exitFile)

	// 通过 stdin 发送命令
	session.mu.Lock()
	_, err := session.StdinWriter.Write([]byte(cmdWrapper + "\n"))
	session.mu.Unlock()

	if err != nil {
		return nil, &SessionError{Op: "execute", Err: err}
	}

	if async {
		return &SessionExecuteResponse{CommandId: cmdId}, nil
	}

	// 同步等待结果
	return m.waitForCommand(sessionCommand)
}


// waitForCommand 等待命令完成
func (m *SessionManager) waitForCommand(sessionCommand *SessionCommand) (*SessionExecuteResponse, error) {
	deadline := time.Now().Add(DefaultTimeout)

	for time.Now().Before(deadline) {
		// 检查 exit_code 文件
		exitCodeBytes, err := os.ReadFile(sessionCommand.ExitFile)
		if err != nil {
			if os.IsNotExist(err) {
				time.Sleep(PollingInterval)
				continue
			}
			return nil, &SessionError{Op: "wait", Err: err}
		}

		// 解析退出码
		exitCodeStr := strings.TrimSpace(string(exitCodeBytes))
		exitCode, err := strconv.Atoi(exitCodeStr)
		if err != nil {
			return nil, &SessionError{Op: "wait", Err: err}
		}

		// 读取输出
		// 注意：当命令包含重定向时，output.log 可能不存在，此时返回空输出
		var output string
		outputBytes, err := os.ReadFile(sessionCommand.LogFile)
		if err == nil {
			output = string(outputBytes)
		}

		sessionCommand.ExitCode = &exitCode
		return &SessionExecuteResponse{
			CommandId: sessionCommand.ID,
			Output:    &output,
			ExitCode:  &exitCode,
		}, nil
	}

	// 超时
	return nil, &SessionError{Op: "wait", Err: fmt.Errorf("command timeout")}
}

// GetCommand 获取命令结果
func (m *SessionManager) GetCommand(sessionId, cmdId string) (*SessionCommand, error) {
	session, ok := m.Get(sessionId)
	if !ok {
		return nil, &SessionError{Op: "get_command", Err: ErrSessionNotFound}
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	cmd, ok := session.Commands[cmdId]
	if !ok {
		return nil, &SessionError{Op: "get_command", Err: fmt.Errorf("command not found")}
	}

	return cmd, nil
}

// GetCommandOutput 获取命令输出
func (m *SessionManager) GetCommandOutput(sessionId, cmdId string) ([]byte, error) {
	session, ok := m.Get(sessionId)
	if !ok {
		return nil, &SessionError{Op: "get_output", Err: ErrSessionNotFound}
	}

	session.mu.RLock()
	cmd, ok := session.Commands[cmdId]
	session.mu.RUnlock()

	if !ok {
		return nil, &SessionError{Op: "get_output", Err: fmt.Errorf("command not found")}
	}

	return os.ReadFile(cmd.LogFile)
}

// ListCommands 列出 Session 中的所有命令
func (m *SessionManager) ListCommands(sessionId string) ([]*SessionCommand, error) {
	session, ok := m.Get(sessionId)
	if !ok {
		return nil, &SessionError{Op: "list_commands", Err: ErrSessionNotFound}
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	commands := make([]*SessionCommand, 0, len(session.Commands))
	for _, cmd := range session.Commands {
		commands = append(commands, cmd)
	}

	return commands, nil
}

// Cleanup 清理过期 Session
func (m *SessionManager) Cleanup(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for id, session := range m.sessions {
		// 使用 LastActivity 判断是否超时
		if now.Sub(session.LastActivity) > maxAge {
			session.mu.Lock()
			session.Closed = true
			session.mu.Unlock()

			if session.Cmd != nil && session.Cmd.Process != nil {
				session.Cmd.Process.Kill()
			}

			os.RemoveAll(session.Dir)
			delete(m.sessions, id)
		}
	}
}

// StartCleanupGoroutine 启动定期清理超时会话的 goroutine
func (m *SessionManager) StartCleanupGoroutine(ttl time.Duration, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			m.Cleanup(ttl)
		}
	}()
}

// CleanupCommands 清理 Session 中的旧命令
func (m *SessionManager) CleanupCommands(sessionId string, maxCount int) error {
	session, ok := m.Get(sessionId)
	if !ok {
		return &SessionError{Op: "cleanup_commands", Err: ErrSessionNotFound}
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if len(session.Commands) <= maxCount {
		return nil
	}

	// 按时间排序，删除最旧的
	type cmdTime struct {
		id        string
		createdAt time.Time
	}

	times := make([]cmdTime, 0, len(session.Commands))
	for id, cmd := range session.Commands {
		times = append(times, cmdTime{id: id, createdAt: cmd.CreatedAt})
	}

	// 简单选择最旧的 maxCount 个保留
	if len(times) <= maxCount {
		return nil
	}

	// 删除超过限制的最旧命令
	toDelete := len(times) - maxCount
	for i := 0; i < toDelete; i++ {
		oldestIdx := 0
		for j := 1; j < len(times); j++ {
			if times[j].createdAt.Before(times[oldestIdx].createdAt) {
				oldestIdx = j
			}
		}

		cmdId := times[oldestIdx].id
		if cmd, ok := session.Commands[cmdId]; ok {
			os.RemoveAll(filepath.Dir(cmd.LogFile))
			delete(session.Commands, cmdId)
		}

		// 从 times 移除
		times = append(times[:oldestIdx], times[oldestIdx+1:]...)
	}

	return nil
}

// WriteToStdin 写入 stdin
func (m *SessionManager) WriteToStdin(sessionId string, data string) error {
	session, ok := m.Get(sessionId)
	if !ok {
		return &SessionError{Op: "write_stdin", Err: ErrSessionNotFound}
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.Closed {
		return &SessionError{Op: "write_stdin", Err: ErrSessionClosed}
	}

	_, err := session.StdinWriter.Write([]byte(data))
	return err
}

// IsClosed 检查 Session 是否已关闭
func (m *SessionManager) IsClosed(sessionId string) bool {
	session, ok := m.Get(sessionId)
	if !ok {
		return true
	}

	session.mu.RLock()
	defer session.mu.RUnlock()
	return session.Closed
}

// GetSessionDir 获取 Session 目录
func (m *SessionManager) GetSessionDir(sessionId string) string {
	session, ok := m.Get(sessionId)
	if !ok {
		return ""
	}
	return session.Dir
}

// ReadLog 读取命令日志
func ReadLog(logFile string) (string, error) {
	content, err := os.ReadFile(logFile)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// ParseLogOutput 解析日志输出，分离 stdout 和 stderr
func ParseLogOutput(output string) (stdout, stderr string) {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "<stderr>") {
			stderr += strings.TrimPrefix(line, "<stderr>") + "\n"
		} else {
			stdout += line + "\n"
		}
	}
	return strings.TrimRight(stdout, "\n"), strings.TrimRight(stderr, "\n")
}
