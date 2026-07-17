// Copyright 2026 SandrPod Contributors
// HTTP tests for the /process/session routes and executor permission helpers.

package toolbox

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// ---------- session HTTP routes ----------

func TestSessionListHandler_Empty_ReturnsEmptyArray(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/process/session", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var dtos []SessionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dtos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(dtos) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(dtos))
	}
}

func TestSessionGetHandler_Missing_Returns404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/process/session/no-such-id", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Deleting a missing session returns 404. (The handler now matches via
// errors.Is(err, ErrSessionNotFound) instead of a brittle substring on the
// error message, which previously yielded a 500.)
func TestSessionDeleteHandler_Missing_Returns404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodDelete, "/process/session/no-such-id", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSessionCreateHandler_CreatesSession(t *testing.T) {
	requireCmd(t, "bash")
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/process/session", `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var dto SessionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.SessionId == "" {
		t.Error("expected a session_id")
	}
	// Clean up the spawned shell.
	_ = s.sessionManager.Delete(dto.SessionId)
}

func TestSessionCreateThenGetThenDelete_FullCycle(t *testing.T) {
	requireCmd(t, "bash")
	s, _ := newTestServer(t)

	// Create
	rec := doRequest(t, s, http.MethodPost, "/process/session", `{"session_id":"cycle-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}

	// Get
	rec = doRequest(t, s, http.MethodGet, "/process/session/cycle-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}

	// List should now include it.
	rec = doRequest(t, s, http.MethodGet, "/process/session", "")
	var dtos []SessionDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &dtos)
	if len(dtos) != 1 {
		t.Errorf("list = %d sessions, want 1", len(dtos))
	}

	// Delete
	rec = doRequest(t, s, http.MethodDelete, "/process/session/cycle-1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}

	// Gone
	rec = doRequest(t, s, http.MethodGet, "/process/session/cycle-1", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404", rec.Code)
	}
}

// ---------- executor permission helpers ----------

func TestExecutor_PermissionManager_DefaultNil(t *testing.T) {
	e := newTestExecutor(t)
	if e.PermissionManager() != nil {
		t.Error("default PermissionManager should be nil")
	}
}

func TestExecutor_SetPermissionManager_Nil_NoPanic(t *testing.T) {
	e := newTestExecutor(t)
	e.SetPermissionManager(nil)
	if e.PermissionManager() != nil {
		t.Error("PermissionManager should remain nil")
	}
}

func TestExecutor_GetWorkDirForPermission_ReturnsWorkDir(t *testing.T) {
	e := newTestExecutor(t)
	if got := e.GetWorkDirForPermission(); got != e.workDir {
		t.Errorf("GetWorkDirForPermission = %q, want %q", got, e.workDir)
	}
}

func TestWithSandboxSession_RoundTrips(t *testing.T) {
	ctx := WithSandboxSession(context.Background(), "sess-42")
	if got := sessionFromContext(ctx); got != "sess-42" {
		t.Errorf("sessionFromContext = %q, want sess-42", got)
	}
}

func TestWithSandboxSession_EmptyID_ReturnsSameContext(t *testing.T) {
	base := context.Background()
	ctx := WithSandboxSession(base, "")
	if got := sessionFromContext(ctx); got != "" {
		t.Errorf("sessionFromContext = %q, want empty", got)
	}
}

func TestSessionFromContext_NoValue_ReturnsEmpty(t *testing.T) {
	if got := sessionFromContext(context.Background()); got != "" {
		t.Errorf("sessionFromContext = %q, want empty", got)
	}
}

// resolveAndAuthorize with no permission manager should behave like
// resolveSafePath: allow paths under workDir, deny blacklisted system paths.
func TestExecutor_ResolveAndAuthorize_NoManager_AllowsWorkDir(t *testing.T) {
	e := newTestExecutor(t)
	safe, err := e.resolveAndAuthorize(context.Background(), "file.txt", permission.ModeRead, "test")
	if err != nil {
		t.Fatalf("resolveAndAuthorize: %v", err)
	}
	if !strings.HasPrefix(safe, resolveSymlinks(e.workDir)) {
		t.Errorf("resolved %q not under workDir %q", safe, e.workDir)
	}
}
