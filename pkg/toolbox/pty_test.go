//go:build !windows

// Copyright 2026 SandrPod Contributors
// Tests for the Unix PtyManager. CreateSession spawns a login shell, so the
// lifecycle tests require bash and skip when it is unavailable.

package toolbox

import (
	"strings"
	"testing"
)

func TestPtyManager_GetSession_Missing_ReturnsFalse(t *testing.T) {
	m := NewPtyManager()
	if _, ok := m.GetSession("nope"); ok {
		t.Error("GetSession(missing) should return ok=false")
	}
}

func TestPtyManager_CloseSession_Missing_ReturnsError(t *testing.T) {
	m := NewPtyManager()
	if err := m.CloseSession("nope"); err == nil {
		t.Error("CloseSession(missing) should error")
	}
}

func TestPtyManager_ResizeSession_Missing_ReturnsError(t *testing.T) {
	m := NewPtyManager()
	if err := m.ResizeSession("nope", 80, 24); err == nil {
		t.Error("ResizeSession(missing) should error")
	}
}

func TestPtyManager_ListSessions_Empty(t *testing.T) {
	m := NewPtyManager()
	if got := m.ListSessions(); len(got) != 0 {
		t.Errorf("ListSessions() = %d, want 0", len(got))
	}
}

func TestPtyManager_CreateGetResizeClose_Lifecycle(t *testing.T) {
	requireCmd(t, "bash")
	t.Setenv("SHELL", "/bin/bash")
	m := NewPtyManager()

	sess, err := m.CreateSession(80, 24)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID == "" {
		t.Error("session ID should be set")
	}

	got, ok := m.GetSession(sess.ID)
	if !ok || got.ID != sess.ID {
		t.Errorf("GetSession returned ok=%v", ok)
	}

	if list := m.ListSessions(); len(list) != 1 {
		t.Errorf("ListSessions() = %d, want 1", len(list))
	}

	if err := m.ResizeSession(sess.ID, 120, 40); err != nil {
		t.Errorf("ResizeSession: %v", err)
	}
	if got, _ := m.GetSession(sess.ID); got.Width != 120 || got.Height != 40 {
		t.Errorf("after resize width/height = %d/%d, want 120/40", got.Width, got.Height)
	}

	if err := m.CloseSession(sess.ID); err != nil {
		t.Errorf("CloseSession: %v", err)
	}
	if _, ok := m.GetSession(sess.ID); ok {
		t.Error("session should be gone after CloseSession")
	}
}

func TestPtyHandler_CreateSession_ReturnsID(t *testing.T) {
	requireCmd(t, "bash")
	t.Setenv("SHELL", "/bin/bash")
	h := NewPtyHandler()
	id, err := h.CreateSession(80, 24)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty session ID")
	}
	// Clean up the spawned shell.
	_ = h.manager.CloseSession(id)
}

// A browser terminal resizes with a JSON control frame. It must be recognised
// as a control message and never written to the shell — the first version of
// the portal's terminal printed “{"type":"resize","cols":104,"rows":32}“ on
// the user's prompt for exactly this reason.
func TestParseResize_ControlFrames(t *testing.T) {
	cases := []struct {
		name string
		in   string
		cols int
		rows int
		ok   bool
	}{
		{"browser frame", `{"type":"resize","cols":104,"rows":32}`, 104, 32, true},
		{"whitespace", "{\"type\": \"resize\", \"cols\": 80, \"rows\": 24}", 80, 24, true},
		{"not json", "ls -la\r", 0, 0, false},
		{"other type", `{"type":"ping"}`, 0, 0, false},
		{"zero cols", `{"type":"resize","cols":0,"rows":24}`, 0, 0, false},
		{"absurd", `{"type":"resize","cols":99999,"rows":24}`, 0, 0, false},
		{"json but not an object", `[1,2,3]`, 0, 0, false},
		{"big document", `{"type":"resize","cols":80,"rows":24,"pad":"` + strings.Repeat("x", 400) + `"}`, 0, 0, false},
	}
	for _, tc := range cases {
		cols, rows, ok := parseResize([]byte(tc.in))
		if ok != tc.ok || cols != tc.cols || rows != tc.rows {
			t.Errorf("%s: got (%d,%d,%v), want (%d,%d,%v)", tc.name, cols, rows, ok, tc.cols, tc.rows, tc.ok)
		}
	}
}
