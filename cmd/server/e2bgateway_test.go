package main

import (
	"strings"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

// TestE2BAuthenticator_EnvdToken locks in that the E2B gateway's authenticator
// accepts a sandbox's issued envd access token (stored in a label) as well as
// server tokens. Without it, every envd / code-interpreter call 401s once the
// server has auth enabled — the exact regression that only surfaces against an
// authed server (the local, auth-off harness never exercised this path).
func TestE2BAuthenticator_EnvdToken(t *testing.T) {
	envdTok := "e2b_" + strings.Repeat("d", 40)
	stores := store.NewMemoryStores()
	_ = stores.Sandboxes.Add(&podpkg.SandboxInfo{
		Name:   "e2bxyz",
		Owner:  "alice",
		State:  podpkg.StateRunning,
		Labels: map[string]string{e2bEnvdTokenLabel: envdTok},
	})
	auth := e2bDeps{cfg: serverConfig{Token: "server-tok"}, sandboxes: stores.Sandboxes}.authenticator()

	tests := []struct {
		name   string
		key    string
		wantID string
		wantOK bool
	}{
		{"server token maps to admin", "server-tok", "admin", true},
		{"envd token maps to sandbox owner", envdTok, "alice", true},
		{"unknown key rejected", "nope", "", false},
		{"empty key rejected", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := auth(tc.key)
			if ok != tc.wantOK || id != tc.wantID {
				t.Errorf("auth(%q) = (%q, %v), want (%q, %v)", tc.key, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
