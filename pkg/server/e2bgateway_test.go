package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

// TestAnonymousThrottleKeysByIPAndSandbox makes sure one sandbox's preview
// traffic does not spend another sandbox's budget, and that an IP rotating its
// X-Forwarded-For cannot escape the bucket when it is not a trusted proxy.
func TestAnonymousThrottleKeysByIPAndSandbox(t *testing.T) {
	d := e2bDeps{anonymous: newRateLimiter(1)} // burst 2
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.7:5000"

	if !d.anonymousThrottle(req, "sb1") || !d.anonymousThrottle(req, "sb1") {
		t.Fatal("burst should allow two requests")
	}
	if d.anonymousThrottle(req, "sb1") {
		t.Fatal("third request from the same IP and sandbox should be limited")
	}
	if !d.anonymousThrottle(req, "sb2") {
		t.Fatal("another sandbox must have its own bucket")
	}
	req.RemoteAddr = "198.51.100.8:5000"
	if !d.anonymousThrottle(req, "sb1") {
		t.Fatal("another IP must have its own bucket")
	}

	// A caller-supplied X-Forwarded-For on a public peer is not an identity:
	// after the peer's bucket is spent, a new header value must not refill it.
	req.RemoteAddr = "198.51.100.9:5000"
	if !d.anonymousThrottle(req, "sb1") || !d.anonymousThrottle(req, "sb1") {
		t.Fatal("burst should allow two requests for the new peer")
	}
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if d.anonymousThrottle(req, "sb1") {
		t.Fatal("spoofing X-Forwarded-For must not create a new bucket")
	}
}

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
	auth := e2bDeps{cfg: Config{Token: "server-tok"}, sandboxes: stores.Sandboxes}.authenticator()

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
			id, _, ok := auth(tc.key)
			if ok != tc.wantOK || id != tc.wantID {
				t.Errorf("auth(%q) = (%q, %v), want (%q, %v)", tc.key, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
