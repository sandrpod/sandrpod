//go:build !windows

// Copyright 2024 SandrPod
// PTY Session Manager - 交互式 Shell 会话管理 (Unix/macOS)

package toolbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// PtySession PTY 会话
type PtySession struct {
	ID           string
	Pty          *os.File
	Cmd          *exec.Cmd
	Width        int
	Height       int
	StartedAt    time.Time
	LastActivity time.Time
	closed       atomic.Bool
}

// PtyManager PTY 会话管理器
type PtyManager struct {
	mu       sync.RWMutex
	sessions map[string]*PtySession
}

// NewPtyManager 创建 PTY 管理器
func NewPtyManager() *PtyManager {
	return &PtyManager{
		sessions: make(map[string]*PtySession),
	}
}

// CreateSession 创建新的 PTY 会话
func (m *PtyManager) CreateSession(width, height int) (*PtySession, error) {
	// 创建 PTY
	cmd := exec.Command("/bin/bash", "-l")
	cmd.Env = []string{"TERM=xterm-256color"}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	// 设置终端大小
	pty.Setsize(ptmx, &pty.Winsize{
		Rows: uint16(height),
		Cols: uint16(width),
	})

	sessionID := fmt.Sprintf("pty-%d", time.Now().UnixNano())
	session := &PtySession{
		ID:           sessionID,
		Pty:          ptmx,
		Cmd:          cmd,
		Width:        width,
		Height:       height,
		StartedAt:    time.Now(),
		LastActivity: time.Now(),
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	return session, nil
}

// GetSession 获取会话
func (m *PtyManager) GetSession(id string) (*PtySession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[id]
	return session, ok
}

// CloseSession 关闭会话
func (m *PtyManager) CloseSession(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session not found")
	}
	delete(m.sessions, id)
	// Only close if not already closed
	if session.closed.CompareAndSwap(false, true) {
		session.Pty.Close()
		if session.Cmd != nil && session.Cmd.Process != nil {
			session.Cmd.Process.Kill()
		}
	}
	return nil
}

// ResizeSession 调整终端大小
func (m *PtyManager) ResizeSession(id string, width, height int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session not found")
	}
	session.Width = width
	session.Height = height
	return pty.Setsize(session.Pty, &pty.Winsize{
		Rows: uint16(height),
		Cols: uint16(width),
	})
}

// ListSessions 列出所有会话
func (m *PtyManager) ListSessions() []*PtySession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions := make([]*PtySession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// PtyHandler PTY WebSocket 处理器
type PtyHandler struct {
	manager *PtyManager
}

// NewPtyHandler 创建 PTY 处理器
func NewPtyHandler() *PtyHandler {
	return &PtyHandler{
		manager: NewPtyManager(),
	}
}

// CreateSession 处理创建 PTY 会话请求
func (h *PtyHandler) CreateSession(width, height int) (string, error) {
	session, err := h.manager.CreateSession(width, height)
	if err != nil {
		return "", err
	}
	return session.ID, nil
}

// HandleWebSocket 处理 WebSocket 连接
func (h *PtyHandler) HandleWebSocket(conn *websocket.Conn, sessionID string) error {
	session, ok := h.manager.GetSession(sessionID)
	if !ok {
		return fmt.Errorf("session not found")
	}

	session.LastActivity = time.Now()

	// 双向复制: WebSocket <-> PTY
	done := make(chan struct{})
	closeOnce := sync.Once{}

	// 从 PTY 读取输出发送到 WebSocket
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := session.Pty.Read(buf)
			if err != nil {
				if err != io.EOF {
					fmt.Printf("PTY read error: %v\n", err)
				}
				closeOnce.Do(func() { close(done) })
				return
			}
			session.LastActivity = time.Now()
			if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				fmt.Printf("WebSocket write error: %v\n", err)
				closeOnce.Do(func() { close(done) })
				return
			}
		}
	}()

	// 从 WebSocket 读取输入发送到 PTY
	go func() {
		for {
			msgType, msg, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					fmt.Printf("WebSocket read error: %v\n", err)
				}
				closeOnce.Do(func() { close(done) })
				return
			}

			if msgType == websocket.CloseMessage {
				closeOnce.Do(func() { close(done) })
				return
			}

			session.LastActivity = time.Now()

			// 处理终端 resize 命令
			if len(msg) >= 6 && msg[0] == 0xFF && msg[1] == 0xFD && msg[2] == 0x1C {
				// IAC SB NAWS <Cols> <Rows> IAC SE
				if len(msg) >= 8 && msg[5] == 0xFF && msg[6] == 0xFF {
					width := int(msg[3])<<8 | int(msg[4])
					height := int(msg[5])<<8 | int(msg[6])
					h.manager.ResizeSession(sessionID, width, height)
					continue
				}
			}

			// 写入 PTY
			if _, err := session.Pty.Write(msg); err != nil {
				fmt.Printf("PTY write error: %v\n", err)
				closeOnce.Do(func() { close(done) })
				return
			}
		}
	}()

	<-done
	// 关闭 WebSocket 连接时会关闭会话
	h.manager.CloseSession(sessionID)
	return nil
}

// CloseSession 关闭会话
func (h *PtyHandler) CloseSession(sessionID string) error {
	return h.manager.CloseSession(sessionID)
}