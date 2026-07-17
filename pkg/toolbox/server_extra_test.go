// Copyright 2026 SandrPod Contributors
// Additional coverage: server lifecycle, multipart uploads, session exec/command.

package toolbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- Server lifecycle ----------

func TestServer_Executor_NonNil(t *testing.T) {
	s := NewServer("", "")
	if s.Executor() == nil {
		t.Fatal("Executor() returned nil")
	}
}

func TestServer_StartAndStop(t *testing.T) {
	// Bind to an ephemeral port to avoid collisions.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	s := NewServer(addr, "")
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	// Poll /health until the server is up.
	base := "http://" + addr
	var resp *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get(base + "/health")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never became ready: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Stop should shut the server down; Start returns http.ErrServerClosed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Errorf("Stop: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Start returned %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Start did not return after Stop")
	}
}

// ---------- multipart upload helpers ----------

func newMultipartBody(t *testing.T, fieldFiles map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for field, content := range fieldFiles {
		fw, err := mw.CreateFormFile(field, field)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return body, mw.FormDataContentType()
}

func TestFilesUploadHandler_WritesFile(t *testing.T) {
	s, dir := newTestServer(t)
	body, ct := newMultipartBody(t, map[string]string{"file": "uploaded-content"})

	req := httptest.NewRequest(http.MethodPost, "/files/upload?path="+dir, body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The file is named after the multipart field key ("file") since header
	// filename equals the field name here; verify content landed on disk.
	got, err := os.ReadFile(filepath.Join(dir, "file"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(got) != "uploaded-content" {
		t.Errorf("content = %q, want uploaded-content", got)
	}
}

func TestFilesUploadHandler_MissingPath_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	body, ct := newMultipartBody(t, map[string]string{"file": "x"})
	req := httptest.NewRequest(http.MethodPost, "/files/upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFilesUploadHandler_WrongMethod_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/upload", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestFilesBulkUploadHandler_WritesFiles(t *testing.T) {
	s, dir := newTestServer(t)
	body, ct := newMultipartBody(t, map[string]string{
		"one.txt": "first",
		"two.txt": "second",
	})

	req := httptest.NewRequest(http.MethodPost, "/files/bulk-upload?path="+dir, body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool             `json:"success"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Errorf("results = %d, want 2", len(resp.Results))
	}
	if _, err := os.Stat(filepath.Join(dir, "one.txt")); err != nil {
		t.Errorf("one.txt not written: %v", err)
	}
}

func TestFilesBulkUploadHandler_MissingPath_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	body, ct := newMultipartBody(t, map[string]string{"a": "x"})
	req := httptest.NewRequest(http.MethodPost, "/files/bulk-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---------- session exec + command over HTTP ----------

func TestSessionExecHandler_RunsCommandViaHTTP(t *testing.T) {
	requireCmd(t, "bash")
	s, _ := newTestServer(t)

	// Create a session.
	rec := doRequest(t, s, http.MethodPost, "/process/session", `{"session_id":"exec-sess"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	t.Cleanup(func() { _ = s.sessionManager.Delete("exec-sess") })

	// Execute a command synchronously.
	rec = doRequest(t, s, http.MethodPost, "/process/session/exec-sess/exec",
		`{"command":"echo http-exec"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp SessionExecuteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Output == nil || !strings.Contains(*resp.Output, "http-exec") {
		t.Errorf("Output = %v, want to contain http-exec", resp.Output)
	}
}

func TestSessionExecHandler_MissingCommand_Returns400(t *testing.T) {
	requireCmd(t, "bash")
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/process/session", `{"session_id":"exec-sess2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	t.Cleanup(func() { _ = s.sessionManager.Delete("exec-sess2") })

	rec = doRequest(t, s, http.MethodPost, "/process/session/exec-sess2/exec", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// The command-result route GET /process/session/{id}/command/{cmdId} is
// reachable: its routing branch precedes the generic "{sandboxName}/{sessionId}"
// GET branch that used to shadow it.
func TestSessionCommandHandler_Reachable_Returns200(t *testing.T) {
	requireCmd(t, "bash")
	s, _ := newTestServer(t)
	sess, err := s.sessionManager.Create("cmd-sess")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = s.sessionManager.Delete(sess.ID) })

	resp, err := s.sessionManager.Execute(sess.ID, "mycmd", "echo recorded", false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	rec := doRequest(t, s, http.MethodGet,
		fmt.Sprintf("/process/session/%s/command/%s", sess.ID, resp.CommandId), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (command route should be reachable); body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- SessionManager.GetCommandOutput / CleanupCommands ----------

func TestSessionManager_GetCommandOutput_ReturnsBytes(t *testing.T) {
	requireCmd(t, "bash")
	m := newTestSessionManager(t)
	sess, err := m.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(sess.ID) })

	resp, err := m.Execute(sess.ID, "c1", "echo out-bytes", false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, err := m.GetCommandOutput(sess.ID, resp.CommandId)
	if err != nil {
		t.Fatalf("GetCommandOutput: %v", err)
	}
	if !strings.Contains(string(out), "out-bytes") {
		t.Errorf("output = %q, want to contain out-bytes", out)
	}
}

func TestSessionManager_GetCommandOutput_UnknownSession_ReturnsError(t *testing.T) {
	m := newTestSessionManager(t)
	if _, err := m.GetCommandOutput("nope", "c1"); err == nil {
		t.Error("GetCommandOutput on missing session should error")
	}
}

func TestSessionManager_CleanupCommands_UnknownSession_ReturnsError(t *testing.T) {
	m := newTestSessionManager(t)
	if err := m.CleanupCommands("nope", 5); err == nil {
		t.Error("CleanupCommands on missing session should error")
	}
}

func TestSessionManager_CleanupCommands_TrimsOldest(t *testing.T) {
	requireCmd(t, "bash")
	m := newTestSessionManager(t)
	sess, err := m.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(sess.ID) })

	// Record three commands.
	for i := range 3 {
		if _, err := m.Execute(sess.ID, fmt.Sprintf("c%d", i), "echo x", false); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	cmds, _ := m.ListCommands(sess.ID)
	if len(cmds) != 3 {
		t.Fatalf("expected 3 commands, got %d", len(cmds))
	}

	if err := m.CleanupCommands(sess.ID, 1); err != nil {
		t.Fatalf("CleanupCommands: %v", err)
	}
	cmds, _ = m.ListCommands(sess.ID)
	if len(cmds) != 1 {
		t.Errorf("after cleanup expected 1 command, got %d", len(cmds))
	}
}

func TestSessionManager_CleanupCommands_BelowLimit_NoOp(t *testing.T) {
	requireCmd(t, "bash")
	m := newTestSessionManager(t)
	sess, err := m.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(sess.ID) })

	if _, err := m.Execute(sess.ID, "only", "echo x", false); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := m.CleanupCommands(sess.ID, 10); err != nil {
		t.Fatalf("CleanupCommands: %v", err)
	}
	cmds, _ := m.ListCommands(sess.ID)
	if len(cmds) != 1 {
		t.Errorf("expected 1 command retained, got %d", len(cmds))
	}
}
