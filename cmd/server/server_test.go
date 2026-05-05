package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// newTestHandler builds a handler with an empty in-memory store and no auth token.
func newTestHandler(t *testing.T, cfg serverConfig) http.Handler {
	t.Helper()
	stores := store.NewMemoryStores()
	ts := tunnel.NewTunnelStore()
	ds := tunnel.NewDirectTunnelStore()
	return buildMux(cfg, stores, ts, ds)
}

// --- 1. /health ---

func TestHealth(t *testing.T) {
	handler := newTestHandler(t, serverConfig{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", body["status"])
	}
}

// --- 2. Auth middleware ---

func TestAuthMiddleware(t *testing.T) {
	const tok = "secret-token"
	handler := newTestHandler(t, serverConfig{Token: tok})

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"no auth header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"correct token", "Bearer " + tok, http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Errorf("expected %d, got %d", tc.wantStatus, w.Code)
			}
		})
	}
}

// --- 3. GET /api/v1/sandboxes (empty store) ---

func TestListSandboxesEmpty(t *testing.T) {
	handler := newTestHandler(t, serverConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	sandboxes, ok := body["sandboxes"]
	if !ok {
		t.Fatal("missing sandboxes key in response")
	}
	slice, ok := sandboxes.([]any)
	if !ok {
		t.Fatalf("sandboxes is not an array: %T", sandboxes)
	}
	if len(slice) != 0 {
		t.Errorf("expected empty slice, got %d elements", len(slice))
	}
}

// --- 4. GET /api/v1/poders (empty store) ---

func TestListPodersEmpty(t *testing.T) {
	handler := newTestHandler(t, serverConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/poders", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	poders, ok := body["poders"]
	if !ok {
		t.Fatal("missing poders key in response")
	}
	slice, ok := poders.([]any)
	if !ok {
		t.Fatalf("poders is not an array: %T", poders)
	}
	if len(slice) != 0 {
		t.Errorf("expected empty slice, got %d elements", len(slice))
	}
}

// --- 5. POST /api/v1/sandboxes (no poder connected) ---
// Scheduler returns error "no available local poder found" → HTTP 500.

func TestCreateSandboxNoPoderAvailable(t *testing.T) {
	handler := newTestHandler(t, serverConfig{APIURL: "http://localhost:8080"})
	body := `{"name":"test-sb","provider_type":"local"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no available local poder") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

// --- 6. GET /api/v1/sandboxes/{nonexistent} ---

func TestGetSandboxNotFound(t *testing.T) {
	handler := newTestHandler(t, serverConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/nonexistent", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- 7. DELETE /api/v1/sandboxes/{nonexistent} ---

func TestDeleteSandboxNotFound(t *testing.T) {
	handler := newTestHandler(t, serverConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/nonexistent", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "sandbox not found") {
		t.Errorf("expected body to contain 'sandbox not found', got: %s", w.Body.String())
	}
}

// --- 8. DELETE /api/v1/poders/{nonexistent} ---

func TestDeletePoderNotFound(t *testing.T) {
	handler := newTestHandler(t, serverConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/poders/nonexistent", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "poder not found") {
		t.Errorf("expected body to contain 'poder not found', got: %s", w.Body.String())
	}
}
