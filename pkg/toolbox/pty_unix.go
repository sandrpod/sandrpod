//go:build !windows

// Copyright 2026 SandrPod Contributors
// PTY Session Manager - interactive shell session management (Unix/macOS)

package toolbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// PtySession holds state for an active PTY-backed terminal session.
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

// PtyManager tracks all active PTY sessions.
type PtyManager struct {
	mu       sync.RWMutex
	sessions map[string]*PtySession
}

// NewPtyManager creates a new PtyManager.
func NewPtyManager() *PtyManager {
	return &PtyManager{
		sessions: make(map[string]*PtySession),
	}
}

// CreateSession starts a new PTY-backed bash session with the given terminal dimensions.
func (m *PtyManager) CreateSession(width, height int) (*PtySession, error) {
	// Allocate a PTY and start the shell.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-l")
	// Inherit the full process environment so login shells can load profile
	// scripts correctly (HOME, PATH, USER, etc.). Override TERM so the
	// terminal behaves consistently regardless of the parent's TERM setting.
	env := os.Environ()
	termSet := false
	for i, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			env[i] = "TERM=xterm-256color"
			termSet = true
			break
		}
	}
	if !termSet {
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = env

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	// Apply the initial terminal size.
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

// GetSession returns the PTY session for the given ID.
func (m *PtyManager) GetSession(id string) (*PtySession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[id]
	return session, ok
}

// CloseSession closes the PTY and kills the underlying process for the given session.
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

// ResizeSession updates the terminal dimensions for an existing PTY session.
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

// ListSessions returns all active PTY sessions.
func (m *PtyManager) ListSessions() []*PtySession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions := make([]*PtySession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// PtyHandler bridges PTY sessions to WebSocket connections.
type PtyHandler struct {
	manager *PtyManager
}

// NewPtyHandler creates a new PtyHandler with its own PtyManager.
func NewPtyHandler() *PtyHandler {
	return &PtyHandler{
		manager: NewPtyManager(),
	}
}

// CreateSession creates a new PTY session and returns its ID.
func (h *PtyHandler) CreateSession(width, height int) (string, error) {
	session, err := h.manager.CreateSession(width, height)
	if err != nil {
		return "", err
	}
	return session.ID, nil
}

// HandleWebSocket bridges a WebSocket connection to a PTY session, proxying data
// in both directions until either side closes the connection.
func (h *PtyHandler) HandleWebSocket(conn *websocket.Conn, sessionID string) error {
	session, ok := h.manager.GetSession(sessionID)
	if !ok {
		return fmt.Errorf("session not found")
	}

	session.LastActivity = time.Now()

	// Bidirectional copy: WebSocket <-> PTY
	done := make(chan struct{})
	closeOnce := sync.Once{}

	// Forward PTY output to the WebSocket client.
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

	// Forward WebSocket input to the PTY.
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

			// Handle terminal resize commands encoded as Telnet NAWS sequences.
			if len(msg) >= 6 && msg[0] == 0xFF && msg[1] == 0xFD && msg[2] == 0x1C {
				// IAC SB NAWS <Cols> <Rows> IAC SE
				if len(msg) >= 8 && msg[5] == 0xFF && msg[6] == 0xFF {
					width := int(msg[3])<<8 | int(msg[4])
					height := int(msg[5])<<8 | int(msg[6])
					h.manager.ResizeSession(sessionID, width, height)
					continue
				}
			}

			// Write the message to the PTY stdin.
			if _, err := session.Pty.Write(msg); err != nil {
				fmt.Printf("PTY write error: %v\n", err)
				closeOnce.Do(func() { close(done) })
				return
			}
		}
	}()

	<-done
	// Close the session when the WebSocket connection ends.
	h.manager.CloseSession(sessionID)
	return nil
}

// CloseSession closes the PTY session with the given ID.
func (h *PtyHandler) CloseSession(sessionID string) error {
	return h.manager.CloseSession(sessionID)
}
