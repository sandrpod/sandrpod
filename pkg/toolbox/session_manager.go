// Copyright 2024 SandrPod
// Session Manager - core session management logic

package toolbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// validIDRe restricts session/command IDs to safe characters to prevent shell injection
var validIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

const (
	// PollingInterval is the interval between exit-code file polls
	PollingInterval = 50 * time.Millisecond
	// DefaultTimeout is the default command execution timeout
	DefaultTimeout = 30 * time.Second
	// DefaultSessionTTL is how long a session stays alive without activity (30 minutes)
	DefaultSessionTTL = 30 * time.Minute
	// CleanupInterval is how often the cleanup goroutine runs
	CleanupInterval = 5 * time.Minute
)

// Execute runs a command inside a session
func (m *SessionManager) Execute(sessionId, cmdId, command string, async bool) (*SessionExecuteResponse, error) {
	// Validate sessionId to prevent shell injection via file paths
	if !validIDRe.MatchString(sessionId) {
		return nil, &SessionError{Op: "execute", Err: fmt.Errorf("invalid session_id")}
	}

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

	// Generate or validate cmdId
	if cmdId == "" {
		cmdId = uuid.NewString()
	} else if !validIDRe.MatchString(cmdId) {
		return nil, &SessionError{Op: "execute", Err: fmt.Errorf("invalid cmd_id")}
	}

	// Create the command working directory
	cmdDir := filepath.Join(session.Dir, cmdId)
	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		return nil, &SessionError{Op: "execute", Err: err}
	}

	logFile := filepath.Join(cmdDir, "output.log")
	exitFile := filepath.Join(cmdDir, "exit_code")

	sessionCommand := &SessionCommand{
		ID:        cmdId,
		Command:   command,
		LogFile:   logFile,
		ExitFile:  exitFile,
		CreatedAt: time.Now(),
	}

	// Register command and update last-activity timestamp
	session.mu.Lock()
	session.Commands[cmdId] = sessionCommand
	session.LastActivity = time.Now()
	session.mu.Unlock()

	// Build the command wrapper script (platform function generates bash/PowerShell syntax)
	cmdWrapper := buildCommandWrapper(command, logFile, exitFile)

	// Send the command via stdin
	session.mu.Lock()
	_, err := session.StdinWriter.Write([]byte(cmdWrapper + "\n"))
	session.mu.Unlock()

	if err != nil {
		return nil, &SessionError{Op: "execute", Err: err}
	}

	if async {
		return &SessionExecuteResponse{CommandId: cmdId}, nil
	}

	// Wait synchronously for the result
	return m.waitForCommand(sessionCommand)
}

// waitForCommand polls until the command writes an exit code file
func (m *SessionManager) waitForCommand(sessionCommand *SessionCommand) (*SessionExecuteResponse, error) {
	deadline := time.Now().Add(DefaultTimeout)

	for time.Now().Before(deadline) {
		// Check for the exit_code file
		exitCodeBytes, err := os.ReadFile(sessionCommand.ExitFile)
		if err != nil {
			if os.IsNotExist(err) {
				time.Sleep(PollingInterval)
				continue
			}
			return nil, &SessionError{Op: "wait", Err: err}
		}

		// Parse exit code
		exitCodeStr := strings.TrimSpace(string(exitCodeBytes))
		exitCode, err := strconv.Atoi(exitCodeStr)
		if err != nil {
			return nil, &SessionError{Op: "wait", Err: err}
		}

		// Read command output.
		// Note: output.log may not exist when the command uses its own redirections; return empty in that case.
		var output string
		outputBytes, err := os.ReadFile(sessionCommand.LogFile)
		if err == nil {
			output = string(stripBOM(outputBytes))
		}

		sessionCommand.ExitCode = &exitCode
		return &SessionExecuteResponse{
			CommandId: sessionCommand.ID,
			Output:    &output,
			ExitCode:  &exitCode,
		}, nil
	}

	// Deadline exceeded
	return nil, &SessionError{Op: "wait", Err: fmt.Errorf("command timeout")}
}

// GetCommand retrieves a command record from a session
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

// GetCommandOutput returns the raw output bytes for a completed command
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

// ListCommands returns all commands recorded in a session
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

// Cleanup removes sessions that have been idle longer than maxAge
func (m *SessionManager) Cleanup(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for id, session := range m.sessions {
		// Use LastActivity to detect idle sessions
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

// StartCleanupGoroutine starts a background goroutine that periodically evicts idle sessions.
// The goroutine exits automatically when ctx is cancelled.
func (m *SessionManager) StartCleanupGoroutine(ctx context.Context, ttl time.Duration, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.Cleanup(ttl)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// CleanupCommands removes old commands from a session, keeping at most maxCount
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

	// Sort by creation time and delete the oldest entries
	type cmdTime struct {
		id        string
		createdAt time.Time
	}

	times := make([]cmdTime, 0, len(session.Commands))
	for id, cmd := range session.Commands {
		times = append(times, cmdTime{id: id, createdAt: cmd.CreatedAt})
	}

	// Keep the most recent maxCount commands
	if len(times) <= maxCount {
		return nil
	}

	// Remove the oldest commands beyond the limit
	toDelete := len(times) - maxCount
	for range toDelete {
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

		// Remove processed entry from the working slice
		times = append(times[:oldestIdx], times[oldestIdx+1:]...)
	}

	return nil
}

// WriteToStdin sends raw data to the session's stdin
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

// IsClosed reports whether the session has been closed
func (m *SessionManager) IsClosed(sessionId string) bool {
	session, ok := m.Get(sessionId)
	if !ok {
		return true
	}

	session.mu.RLock()
	defer session.mu.RUnlock()
	return session.Closed
}

// GetSessionDir returns the working directory path for a session
func (m *SessionManager) GetSessionDir(sessionId string) string {
	session, ok := m.Get(sessionId)
	if !ok {
		return ""
	}
	return session.Dir
}

// ReadLog reads the contents of a command log file
func ReadLog(logFile string) (string, error) {
	content, err := os.ReadFile(logFile)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// ParseLogOutput splits log output into stdout and stderr streams
func ParseLogOutput(output string) (stdout, stderr string) {
	for line := range strings.SplitSeq(output, "\n") {
		if val, ok := strings.CutPrefix(line, "<stderr>"); ok {
			stderr += val + "\n"
		} else {
			stdout += line + "\n"
		}
	}
	return strings.TrimRight(stdout, "\n"), strings.TrimRight(stderr, "\n")
}

// stripBOM removes a leading UTF-8 BOM (EF BB BF) if present.
// PowerShell 5.x Out-File -Encoding UTF8 prepends a BOM; Go strings are
// BOM-unaware, so we strip it before returning to callers.
func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}
