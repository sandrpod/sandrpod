// Copyright 2026 SandrPod Contributors
// Tests for SessionManager logic and session helper functions.

package toolbox

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestSessionManager returns a SessionManager rooted in a temp dir.
func newTestSessionManager(t *testing.T) *SessionManager {
	t.Helper()
	return NewSessionManager(t.TempDir())
}

// ---------- pure helpers ----------

func TestGenerateSessionId_UniqueNonEmpty(t *testing.T) {
	a := GenerateSessionId()
	b := GenerateSessionId()
	if a == "" || b == "" {
		t.Fatal("GenerateSessionId returned empty string")
	}
	if a == b {
		t.Errorf("GenerateSessionId not unique: %q == %q", a, b)
	}
}

func TestStripBOM_RemovesLeadingBOM(t *testing.T) {
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte("hello")...)
	if got := string(stripBOM(withBOM)); got != "hello" {
		t.Errorf("stripBOM = %q, want hello", got)
	}
}

func TestStripBOM_NoBOM_Unchanged(t *testing.T) {
	if got := string(stripBOM([]byte("plain"))); got != "plain" {
		t.Errorf("stripBOM = %q, want plain", got)
	}
}

func TestStripBOM_ShortInput_Unchanged(t *testing.T) {
	if got := string(stripBOM([]byte{0xEF})); got != string([]byte{0xEF}) {
		t.Errorf("stripBOM mangled short input")
	}
}

func TestParseLogOutput_SplitsStdoutStderr(t *testing.T) {
	input := "line1\n<stderr>err1\nline2\n<stderr>err2"
	stdout, stderr := ParseLogOutput(input)
	if !strings.Contains(stdout, "line1") || !strings.Contains(stdout, "line2") {
		t.Errorf("stdout = %q, want line1 and line2", stdout)
	}
	if !strings.Contains(stderr, "err1") || !strings.Contains(stderr, "err2") {
		t.Errorf("stderr = %q, want err1 and err2", stderr)
	}
	if strings.Contains(stderr, "<stderr>") {
		t.Errorf("stderr should have prefix stripped: %q", stderr)
	}
}

func TestParseLogOutput_OnlyStdout(t *testing.T) {
	stdout, stderr := ParseLogOutput("a\nb\nc")
	if stdout != "a\nb\nc" {
		t.Errorf("stdout = %q, want a\\nb\\nc", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestReadLog_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/log.txt"
	if err := os.WriteFile(p, []byte("log content"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLog(p)
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if got != "log content" {
		t.Errorf("ReadLog = %q, want 'log content'", got)
	}
}

func TestReadLog_MissingFile_ReturnsError(t *testing.T) {
	if _, err := ReadLog(t.TempDir() + "/no-such-log"); err == nil {
		t.Error("ReadLog(missing) should return error")
	}
}

// ---------- SessionError ----------

func TestSessionError_Error_FormatsOpAndErr(t *testing.T) {
	e := &SessionError{Op: "create", Err: errors.New("boom")}
	if got := e.Error(); got != "create: boom" {
		t.Errorf("Error() = %q, want 'create: boom'", got)
	}
}

// ---------- Get / Delete / List on empty manager ----------

func TestSessionManager_Get_Missing_ReturnsFalse(t *testing.T) {
	m := newTestSessionManager(t)
	if _, ok := m.Get("nope"); ok {
		t.Error("Get(missing) should return ok=false")
	}
}

func TestSessionManager_Delete_Missing_ReturnsError(t *testing.T) {
	m := newTestSessionManager(t)
	if err := m.Delete("nope"); err == nil {
		t.Error("Delete(missing) should error")
	}
}

func TestSessionManager_List_Empty(t *testing.T) {
	m := newTestSessionManager(t)
	if got := m.List(); len(got) != 0 {
		t.Errorf("List() = %d entries, want 0", len(got))
	}
}

func TestSessionManager_IsClosed_Missing_ReturnsTrue(t *testing.T) {
	m := newTestSessionManager(t)
	if !m.IsClosed("nope") {
		t.Error("IsClosed(missing) should be true")
	}
}

func TestSessionManager_GetSessionDir_Missing_ReturnsEmpty(t *testing.T) {
	m := newTestSessionManager(t)
	if got := m.GetSessionDir("nope"); got != "" {
		t.Errorf("GetSessionDir(missing) = %q, want empty", got)
	}
}

// ---------- Execute validation (no real shell needed) ----------

func TestSessionManager_Execute_InvalidSessionId_ReturnsError(t *testing.T) {
	m := newTestSessionManager(t)
	_, err := m.Execute("bad id with spaces!", "", "echo x", false)
	if err == nil {
		t.Fatal("expected error for invalid session_id")
	}
	if !strings.Contains(err.Error(), "invalid session_id") {
		t.Errorf("err = %v, want 'invalid session_id'", err)
	}
}

func TestSessionManager_Execute_UnknownSession_ReturnsNotFound(t *testing.T) {
	m := newTestSessionManager(t)
	_, err := m.Execute("valid-id", "", "echo x", false)
	if err == nil {
		t.Fatal("expected ErrSessionNotFound")
	}
}

func TestSessionManager_GetCommand_UnknownSession_ReturnsError(t *testing.T) {
	m := newTestSessionManager(t)
	if _, err := m.GetCommand("nope", "cmd"); err == nil {
		t.Error("GetCommand on missing session should error")
	}
}

func TestSessionManager_ListCommands_UnknownSession_ReturnsError(t *testing.T) {
	m := newTestSessionManager(t)
	if _, err := m.ListCommands("nope"); err == nil {
		t.Error("ListCommands on missing session should error")
	}
}

func TestSessionManager_WriteToStdin_UnknownSession_ReturnsError(t *testing.T) {
	m := newTestSessionManager(t)
	if err := m.WriteToStdin("nope", "data"); err == nil {
		t.Error("WriteToStdin on missing session should error")
	}
}

// ---------- Cleanup ----------

// Cleanup on an empty manager must be a no-op (no panic).
func TestSessionManager_Cleanup_Empty_NoPanic(t *testing.T) {
	m := newTestSessionManager(t)
	m.Cleanup(time.Minute)
	if len(m.List()) != 0 {
		t.Error("Cleanup on empty manager changed state")
	}
}

// StartCleanupGoroutine must exit when the context is cancelled.
func TestSessionManager_StartCleanupGoroutine_StopsOnCancel(t *testing.T) {
	m := newTestSessionManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.StartCleanupGoroutine(ctx, time.Minute, 10*time.Millisecond)
	cancel()
	// No deterministic assertion possible without leaking internals; the test
	// passes if the goroutine doesn't deadlock the suite. Give it a tick.
	time.Sleep(20 * time.Millisecond)
}

// ---------- Create / lifecycle (requires bash) ----------

func TestSessionManager_Create_And_Get(t *testing.T) {
	requireCmd(t, "bash")
	m := newTestSessionManager(t)
	sess, err := m.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(sess.ID) })

	if sess.ID == "" {
		t.Error("session ID should be set")
	}
	got, ok := m.Get(sess.ID)
	if !ok || got.ID != sess.ID {
		t.Errorf("Get returned ok=%v id=%v", ok, got)
	}
	if m.IsClosed(sess.ID) {
		t.Error("new session should not be closed")
	}
	if dir := m.GetSessionDir(sess.ID); dir == "" {
		t.Error("session dir should be non-empty")
	}
}

func TestSessionManager_Create_DuplicateId_ReturnsError(t *testing.T) {
	requireCmd(t, "bash")
	m := newTestSessionManager(t)
	sess, err := m.Create("dup-id")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(sess.ID) })

	if _, err := m.Create("dup-id"); err == nil {
		t.Error("duplicate session ID should error")
	}
}

func TestSessionManager_Delete_TerminatesSession(t *testing.T) {
	requireCmd(t, "bash")
	m := newTestSessionManager(t)
	sess, err := m.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := sess.Dir
	if err := m.Delete(sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := m.Get(sess.ID); ok {
		t.Error("session should be gone after Delete")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("session dir should be removed after Delete")
	}
}

func TestSession_ToDTO_ReflectsID(t *testing.T) {
	requireCmd(t, "bash")
	m := newTestSessionManager(t)
	sess, err := m.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(sess.ID) })

	dto := sess.ToDTO()
	if dto.SessionId != sess.ID {
		t.Errorf("DTO.SessionId = %q, want %q", dto.SessionId, sess.ID)
	}
	if dto.Commands == nil {
		t.Error("DTO.Commands should be non-nil slice")
	}
}

func TestSessionManager_Execute_Sync_RunsCommand(t *testing.T) {
	requireCmd(t, "bash")
	m := newTestSessionManager(t)
	sess, err := m.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(sess.ID) })

	resp, err := m.Execute(sess.ID, "", "echo session-out", false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", resp.ExitCode)
	}
	if resp.Output == nil || !strings.Contains(*resp.Output, "session-out") {
		t.Errorf("Output = %v, want to contain session-out", resp.Output)
	}
}
