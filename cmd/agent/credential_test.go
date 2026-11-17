// Copyright 2026 SandrPod Contributors

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// enrolServer returns a control plane that issues wk_fresh and counts calls.
func enrolServer(t *testing.T, calls *int32, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/poders/enroll" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer jn_test" {
			t.Errorf("authorization = %q", got)
		}
		var req struct {
			PoderID string `json:"poder_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if !strings.HasPrefix(req.PoderID, "agent-") || len(req.PoderID) < 8 {
			t.Errorf("poder id %q is not an agent id", req.PoderID)
		}
		atomic.AddInt32(calls, 1)
		if status != http.StatusOK {
			http.Error(w, "enrolment refused", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "wk_fresh",
			"expires_at": time.Now().Add(180 * 24 * time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveAgentToken_ExchangesAndReuses(t *testing.T) {
	var calls int32
	srv := enrolServer(t, &calls, http.StatusOK)
	dir := t.TempDir()

	tok, id := resolveAgentToken(srv.URL, "jn_test", dir)
	if tok != "wk_fresh" {
		t.Fatalf("token = %q, want the exchanged credential", tok)
	}
	if !strings.HasPrefix(id, "agent-") {
		t.Fatalf("id = %q, want a stable agent id", id)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("enrolment calls = %d, want 1", got)
	}

	stored, ok := readAgentCredential(filepath.Join(dir, agentCredentialFile))
	if !ok || stored.Token != "wk_fresh" || stored.PoderID != id {
		t.Fatalf("credential not persisted: %+v", stored)
	}
	info, err := os.Stat(filepath.Join(dir, agentCredentialFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("credential mode = %o, want 600", mode)
	}

	// A restart presents the same invitation; the stored key must win and must
	// not enroll again.
	tok2, id2 := resolveAgentToken(srv.URL, "jn_test", dir)
	if tok2 != "wk_fresh" || id2 != id {
		t.Fatalf("second resolve = %q/%q, want the stored key", tok2, id2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("enrolment calls after reuse = %d, want 1", got)
	}
}

func TestResolveAgentToken_RenewsExpiredKey(t *testing.T) {
	var calls int32
	srv := enrolServer(t, &calls, http.StatusOK)
	dir := t.TempDir()
	// A key that expired yesterday, with the stable id it was bound to.
	if err := writeAgentCredential(filepath.Join(dir, agentCredentialFile), agentCredential{
		PoderID:   "agent-old",
		Token:     "wk_old",
		ExpiresAt: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	tok, id := resolveAgentToken(srv.URL, "jn_test", dir)
	if tok != "wk_fresh" || id != "agent-old" {
		t.Fatalf("resolve = %q/%q, want a renewal keeping the id", tok, id)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("enrolment calls = %d, want 1", got)
	}
}

func TestResolveAgentToken_StaticTokenPassesThrough(t *testing.T) {
	dir := t.TempDir()
	tok, _ := resolveAgentToken("https://cloud.example.com", "platform-token", dir)
	if tok != "platform-token" {
		t.Fatalf("token = %q, want the static token untouched", tok)
	}
	if _, err := os.Stat(filepath.Join(dir, agentCredentialFile)); !os.IsNotExist(err) {
		t.Fatal("a static token must not create a credential file")
	}
}

func TestResolveAgentToken_RefusalFallsBackToInvitation(t *testing.T) {
	var calls int32
	srv := enrolServer(t, &calls, http.StatusUnauthorized)
	dir := t.TempDir()

	tok, id := resolveAgentToken(srv.URL, "jn_test", dir)
	if tok != "jn_test" {
		t.Fatalf("token = %q, want the invitation as a fallback", tok)
	}
	if id == "" {
		t.Fatal("the generated id must be kept so a retry binds to it")
	}
	if _, err := os.Stat(filepath.Join(dir, agentCredentialFile)); !os.IsNotExist(err) {
		t.Fatal("a refused enrolment must not write a credential")
	}
}

func TestCredentialExpired(t *testing.T) {
	if credentialExpired("") {
		t.Error("an empty expiry (static token) must not be treated as expired")
	}
	if !credentialExpired("not-a-time") {
		t.Error("an unreadable expiry must be treated as expired")
	}
	if !credentialExpired(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)) {
		t.Error("a past expiry was not detected")
	}
	if credentialExpired(time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)) {
		t.Error("a future expiry was treated as expired")
	}
}
