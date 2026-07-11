package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTrayAuth guards the settings-page token gate: without the per-session
// token every route is forbidden (closing the toolbox-port-proxy → tray
// pivot), and the token is accepted via either the query param or the header.
func TestTrayAuth(t *testing.T) {
	const token = "secret-session-token"
	h := trayAuth(token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	do := func(target, header string) int {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		if header != "" {
			req.Header.Set("X-Tray-Token", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do("/api/rules/add", ""); got != http.StatusForbidden {
		t.Errorf("no token: got %d, want 403 (tray pivot must be blocked)", got)
	}
	if got := do("/api/rules/add", "wrong"); got != http.StatusForbidden {
		t.Errorf("wrong token: got %d, want 403", got)
	}
	if got := do("/?t="+token, ""); got != http.StatusOK {
		t.Errorf("query token: got %d, want 200", got)
	}
	if got := do("/api/rules/add", token); got != http.StatusOK {
		t.Errorf("header token: got %d, want 200", got)
	}
}
