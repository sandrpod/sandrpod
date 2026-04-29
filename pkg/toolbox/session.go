// Copyright 2024 SandrPod
// Session - persistent shell session management

package toolbox

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session represents a persistent shell session backed by a long-lived process.
type Session struct {
	ID           string
	Cmd          *exec.Cmd
	StdinWriter  io.WriteCloser
	Commands     map[string]*SessionCommand
	Dir          string
	CreatedAt    time.Time
	LastActivity time.Time // updated on each command; used for TTL-based cleanup
	Closed       bool
	mu           sync.RWMutex
}

// SessionCommand represents a single command executed within a session.
type SessionCommand struct {
	ID       string
	Command  string
	ExitCode *int
	LogFile  string
	ExitFile string
	CreatedAt time.Time
}

// SessionManager manages the lifecycle of all active sessions.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	baseDir  string
}

// SessionExecuteRequest is the payload for executing a command in a session.
type SessionExecuteRequest struct {
	SessionId string `json:"session_id,omitempty"`
	Command   string `json:"command"`
	Async     bool   `json:"async"`
}

// SessionExecuteResponse is the response returned after executing a command.
type SessionExecuteResponse struct {
	CommandId string  `json:"cmd_id"`
	Output    *string `json:"output,omitempty"`
	ExitCode  *int    `json:"exit_code,omitempty"`
	Stdout    *string `json:"stdout,omitempty"`
	Stderr    *string `json:"stderr,omitempty"`
}

// SessionDTO is the API response shape for a session.
type SessionDTO struct {
	SessionId string               `json:"session_id"`
	Commands  []*SessionCommandDTO `json:"commands"`
	CreatedAt time.Time            `json:"created_at"`
}

// SessionCommandDTO is the API response shape for a session command.
type SessionCommandDTO struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateSessionRequest is the payload for creating a new session.
type CreateSessionRequest struct {
	SessionId string `json:"session_id,omitempty"`
}

// NewSessionManager creates a SessionManager rooted at baseDir.
func NewSessionManager(baseDir string) *SessionManager {
	// Ensure the base directory exists.
	os.MkdirAll(baseDir, 0755)
	return &SessionManager{
		sessions: make(map[string]*Session),
		baseDir:  baseDir,
	}
}

// GenerateSessionId returns a new unique session identifier.
func GenerateSessionId() string {
	return uuid.NewString()
}

// Create starts a new persistent shell session. A UUID is generated when sessionId is empty.
func (m *SessionManager) Create(sessionId string) (*Session, error) {
	if sessionId == "" {
		sessionId = GenerateSessionId()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Reject duplicate session IDs.
	if _, ok := m.sessions[sessionId]; ok {
		return nil, &SessionError{Op: "create", Err: ErrSessionExists}
	}

	// Create the per-session working directory.
	sessionDir := m.baseDir + "/" + sessionId
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return nil, &SessionError{Op: "create", Err: err}
	}

	// Launch a persistent shell process (Unix: /bin/bash -i; Windows: powershell.exe -NoExit).
	shell := nativeShell()
	args := nativeShellSessionArgs()
	cmd := exec.Command(shell, args...)
	cmd.Dir = defaultWorkDir()
	cmd.Env = os.Environ()

	// Obtain the stdin pipe before starting the process.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &SessionError{Op: "create", Err: err}
	}

	// Start the shell process.
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

// Get returns the session for the given ID.
func (m *SessionManager) Get(sessionId string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[sessionId]
	return session, ok
}

// Delete terminates a session and removes its working directory.
func (m *SessionManager) Delete(sessionId string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[sessionId]
	if !ok {
		return &SessionError{Op: "delete", Err: ErrSessionNotFound}
	}

	// Mark as closed and kill the underlying process.
	session.mu.Lock()
	session.Closed = true
	session.mu.Unlock()

	if session.Cmd != nil && session.Cmd.Process != nil {
		session.Cmd.Process.Kill()
		session.Cmd.Wait()
	}

	// Remove the session's working directory.
	os.RemoveAll(session.Dir)

	delete(m.sessions, sessionId)
	return nil
}

// List returns all active sessions.
func (m *SessionManager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// ToDTO converts the session to its API response representation.
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

// SessionError wraps a session operation error with the operation name.
type SessionError struct {
	Op  string
	Err error
}

func (e *SessionError) Error() string {
	return e.Op + ": " + e.Err.Error()
}

// Sentinel errors for common session failure cases.
var (
	ErrSessionNotFound = &SessionError{Op: "session", Err: os.ErrNotExist}
	ErrSessionExists   = &SessionError{Op: "session", Err: os.ErrExist}
	ErrSessionClosed   = &SessionError{Op: "session", Err: os.ErrClosed}
)
