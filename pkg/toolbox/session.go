// Copyright 2024 SandrPod
// Session - 持久化 Shell 会话管理

package toolbox

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session 代表一个持久化的 Shell 会话
type Session struct {
	ID           string
	Cmd          *exec.Cmd
	StdinWriter  io.WriteCloser
	Commands     map[string]*SessionCommand
	Dir          string
	CreatedAt    time.Time
	LastActivity time.Time // 最后活动时间，用于 TTL 清理
	Closed       bool
	mu           sync.RWMutex
}

// SessionCommand 代表会话中执行的一个命令
type SessionCommand struct {
	ID       string
	Command  string
	ExitCode *int
	LogFile  string
	ExitFile string
	CreatedAt time.Time
}

// SessionManager 管理所有 Session
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	baseDir  string
}

// SessionExecuteRequest 执行命令请求
type SessionExecuteRequest struct {
	SessionId string `json:"session_id,omitempty"`
	Command   string `json:"command"`
	Async     bool   `json:"async"`
}

// SessionExecuteResponse 执行命令响应
type SessionExecuteResponse struct {
	CommandId string  `json:"cmd_id"`
	Output    *string `json:"output,omitempty"`
	ExitCode  *int    `json:"exit_code,omitempty"`
	Stdout    *string `json:"stdout,omitempty"`
	Stderr    *string `json:"stderr,omitempty"`
}

// SessionDTO API 响应结构
type SessionDTO struct {
	SessionId string               `json:"session_id"`
	Commands  []*SessionCommandDTO `json:"commands"`
	CreatedAt time.Time            `json:"created_at"`
}

// SessionCommandDTO 命令 DTO
type SessionCommandDTO struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateSessionRequest 创建 Session 请求
type CreateSessionRequest struct {
	SessionId string `json:"session_id,omitempty"`
}

// NewSessionManager 创建 Session 管理器
func NewSessionManager(baseDir string) *SessionManager {
	// 确保基础目录存在
	os.MkdirAll(baseDir, 0755)
	return &SessionManager{
		sessions: make(map[string]*Session),
		baseDir:  baseDir,
	}
}

// GenerateSessionId 生成会话 ID
func GenerateSessionId() string {
	return uuid.NewString()
}

// Create 创建新 Session
func (m *SessionManager) Create(sessionId string) (*Session, error) {
	if sessionId == "" {
		sessionId = GenerateSessionId()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查是否已存在
	if _, ok := m.sessions[sessionId]; ok {
		return nil, &SessionError{Op: "create", Err: ErrSessionExists}
	}

	// 创建会话目录
	sessionDir := m.baseDir + "/" + sessionId
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return nil, &SessionError{Op: "create", Err: err}
	}

	// 创建持久 shell 进程（Unix: /bin/bash -i；Windows: powershell.exe -NoExit）
	shell := nativeShell()
	args := nativeShellSessionArgs()
	cmd := exec.Command(shell, args...)
	cmd.Dir = defaultWorkDir()
	cmd.Env = os.Environ()

	// 获取 stdin pipe
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &SessionError{Op: "create", Err: err}
	}

	// 启动进程
	if err := cmd.Start(); err != nil {
		return nil, &SessionError{Op: "create", Err: err}
	}

	session := &Session{
		ID:           sessionId,
		Cmd:          cmd,
		StdinWriter:  stdin,
		Commands:     make(map[string]*SessionCommand),
		Dir:          sessionDir,
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	}

	m.sessions[sessionId] = session
	return session, nil
}

// Get 获取 Session
func (m *SessionManager) Get(sessionId string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[sessionId]
	return session, ok
}

// Delete 删除 Session
func (m *SessionManager) Delete(sessionId string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[sessionId]
	if !ok {
		return &SessionError{Op: "delete", Err: ErrSessionNotFound}
	}

	// 标记关闭并终止进程
	session.mu.Lock()
	session.Closed = true
	session.mu.Unlock()

	if session.Cmd != nil && session.Cmd.Process != nil {
		session.Cmd.Process.Kill()
		session.Cmd.Wait()
	}

	// 清理会话目录
	os.RemoveAll(session.Dir)

	delete(m.sessions, sessionId)
	return nil
}

// List 列出所有 Session
func (m *SessionManager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// ToDTO 转换为 DTO
func (s *Session) ToDTO() *SessionDTO {
	s.mu.RLock()
	defer s.mu.RUnlock()

	commands := make([]*SessionCommandDTO, 0, len(s.Commands))
	for _, cmd := range s.Commands {
		commands = append(commands, &SessionCommandDTO{
			ID:        cmd.ID,
			Command:   cmd.Command,
			ExitCode:  cmd.ExitCode,
			CreatedAt: cmd.CreatedAt,
		})
	}

	return &SessionDTO{
		SessionId: s.ID,
		Commands:  commands,
		CreatedAt: s.CreatedAt,
	}
}

// SessionError 会话错误
type SessionError struct {
	Op  string
	Err error
}

func (e *SessionError) Error() string {
	return e.Op + ": " + e.Err.Error()
}

// 错误定义
var (
	ErrSessionNotFound = &SessionError{Op: "session", Err: os.ErrNotExist}
	ErrSessionExists   = &SessionError{Op: "session", Err: os.ErrExist}
	ErrSessionClosed   = &SessionError{Op: "session", Err: os.ErrClosed}
)
