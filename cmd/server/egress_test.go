// Copyright 2026 SandrPod
// Egress policy over the API: created with the sandbox, readable by the
// sandbox that enforces it, and editable while it runs.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/egress"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

func newEgressTestHandler(t *testing.T, seed *podpkg.SandboxInfo) http.Handler {
	t.Helper()
	stores := store.NewMemoryStores()
	if seed != nil {
		if err := stores.Sandboxes.Add(seed); err != nil {
			t.Fatalf("seed sandbox: %v", err)
		}
	}
	return buildMux(serverConfig{}, stores, tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())
}

func nowUTC() time.Time { return time.Now().UTC() }

func TestEgressPolicy_GetReturnsStoredPolicy(t *testing.T) {
	sb := &podpkg.SandboxInfo{
		Name:         "sb-egress",
		State:        podpkg.StateRunning,
		CreatedAt:    nowUTC(),
		LastActivity: nowUTC(),
		Network: &egress.Policy{
			AllowedDomains:      []string{"api.example.com"},
			DeniedDomains:       []string{"*.internal"},
			DeniedDomainReasons: map[string]string{"*.internal": "use the mirror"},
		},
	}
	handler := newEgressTestHandler(t, sb)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sb-egress/egress", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Sandbox string         `json:"sandbox"`
		Policy  *egress.Policy `json:"policy"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Policy == nil || len(body.Policy.AllowedDomains) != 1 || body.Policy.AllowedDomains[0] != "api.example.com" {
		t.Fatalf("policy = %+v", body.Policy)
	}
	if body.Policy.DeniedDomainReasons["*.internal"] != "use the mirror" {
		t.Errorf("reasons did not round-trip: %+v", body.Policy.DeniedDomainReasons)
	}
}

// A sandbox created without a policy answers with a null policy — distinct
// from an empty one, and the agent turns both into "deny everything".
func TestEgressPolicy_GetWithoutPolicyIsNull(t *testing.T) {
	handler := newEgressTestHandler(t, &podpkg.SandboxInfo{
		Name: "sb-nopolicy", State: podpkg.StateRunning, CreatedAt: nowUTC(), LastActivity: nowUTC(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sb-nopolicy/egress", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"policy":null`) {
		t.Errorf("expected a null policy, got %s", w.Body.String())
	}
}

func TestEgressPolicy_PutReplacesAndValidates(t *testing.T) {
	handler := newEgressTestHandler(t, &podpkg.SandboxInfo{
		Name: "sb-put", State: podpkg.StateRunning, CreatedAt: nowUTC(), LastActivity: nowUTC(),
	})

	// A valid replacement lands.
	body := `{"allowed_domains":["registry.npmjs.org:443"],"denied_resolved_addresses":["10.0.0.0/8"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/sandboxes/sb-put/egress", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sb-put/egress", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "registry.npmjs.org:443") {
		t.Fatalf("stored policy not updated: %s", w.Body.String())
	}

	// A malformed entry is refused, and the old policy stays in force.
	bad := `{"allowed_domains":["http://example.com/path"]}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/sandboxes/sb-put/egress", strings.NewReader(bad))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT with a malformed entry: status = %d, want 400; body = %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sb-put/egress", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "registry.npmjs.org:443") {
		t.Errorf("a rejected PUT must not clear the policy: %s", w.Body.String())
	}
}

func TestEgressPolicy_UnknownSandbox(t *testing.T) {
	handler := newEgressTestHandler(t, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		req := httptest.NewRequest(method, "/api/v1/sandboxes/nope/egress", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s on an unknown sandbox: status = %d, want 404", method, w.Code)
		}
	}
}

// Create-time validation: the operator gets the typo back while they still
// have the create call in front of them, not as a sandbox that provisions and
// then denies everything.
func TestCreateSandbox_RejectsMalformedEgressPolicy(t *testing.T) {
	handler := newEgressTestHandler(t, nil)
	body := `{"name":"sb-bad","region":"local","provider_type":"local","network":{"allowed_domains":["api.*.example.com"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "egress policy") {
		t.Errorf("error should name the policy: %s", w.Body.String())
	}
}

func TestValidateEgressPolicy(t *testing.T) {
	if err := validateEgressPolicy(nil); err != nil {
		t.Errorf("nil policy should be accepted: %v", err)
	}
	if err := validateEgressPolicy(&egress.Policy{AllowedDomains: []string{"ok.example.com"}}); err != nil {
		t.Errorf("valid policy rejected: %v", err)
	}
	for _, bad := range [][]string{
		{"example.com/path"},
		{"*.example.org:99999"},
		{"2001:db8::1:443"},
	} {
		if err := validateEgressPolicy(&egress.Policy{AllowedDomains: bad}); err == nil {
			t.Errorf("allowed_domains %v should be rejected", bad)
		}
	}
}
