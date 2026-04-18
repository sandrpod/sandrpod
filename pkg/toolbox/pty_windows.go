//go:build windows

// Copyright 2024 SandrPod
// PTY Session Manager stub for Windows.
//
// Windows ConPTY is not yet implemented. PTY endpoints return a descriptive
// error so callers fail gracefully. The session API (/process/session/…) is
// the recommended alternative for interactive command execution on Windows.

package toolbox

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// PtySession PTY 会话 (Windows stub)
type PtySession struct {
	ID           string
	Pty          *os.File // always nil on Windows
	Width        int
	Height       int
	StartedAt    time.Time
	LastActivity time.Time
	closed       atomic.Bool
}

// PtyManager PTY 会话管理器 (Windows stub)
type PtyManager struct{}

// NewPtyManager creates a no-op PTY manager on Windows.
func NewPtyManager() *PtyManager { return &PtyManager{} }

func (m *PtyManager) CreateSession(_, _ int) (*PtySession, error) {
	return nil, fmt.Errorf("PTY is not supported on Windows; use the session API (/process/session) instead")
}

func (m *PtyManager) GetSession(_ string) (*PtySession, bool) { return nil, false }

func (m *PtyManager) CloseSession(_ string) error {
	return fmt.Errorf("PTY not supported on Windows")
}

func (m *PtyManager) ResizeSession(_ string, _, _ int) error {
	return fmt.Errorf("PTY not supported on Windows")
}

func (m *PtyManager) ListSessions() []*PtySession { return nil }

// PtyHandler PTY WebSocket 处理器 (Windows stub)
type PtyHandler struct {
	manager *PtyManager
}

// NewPtyHandler creates a no-op PTY handler on Windows.
func NewPtyHandler() *PtyHandler {
	return &PtyHandler{manager: NewPtyManager()}
}

// CreateSession always returns an error on Windows.
func (h *PtyHandler) CreateSession(_, _ int) (string, error) {
	return "", fmt.Errorf("PTY is not supported on Windows; use the session API (/process/session) instead")
}

// HandleWebSocket immediately closes with an error on Windows.
func (h *PtyHandler) HandleWebSocket(conn *websocket.Conn, _ string) error {
	conn.WriteMessage( //nolint:errcheck
		websocket.TextMessage,
		[]byte("PTY is not supported on Windows. Use the session API instead."),
	)
	return fmt.Errorf("PTY not supported on Windows")
}

// CloseSession is a no-op on Windows.
func (h *PtyHandler) CloseSession(_ string) error { return nil }
