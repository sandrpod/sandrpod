// Copyright 2026 SandrPod
// Egress policy over the API: created with the sandbox, readable by the
// sandbox that enforces it, and editable while it runs.

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/e2bcompat"
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
	return New(Config{}, stores, tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())
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

// The per-connection decision endpoint: an agent with -egress-mode=decider
// asks here instead of holding the rules.
func TestPolicyDecide(t *testing.T) {
	handler := newEgressTestHandler(t, &podpkg.SandboxInfo{
		Name: "sb-decide", State: podpkg.StateRunning, CreatedAt: nowUTC(), LastActivity: nowUTC(),
		Network: &egress.Policy{AllowedDomains: []string{"api.example.com", "*.npmjs.org"}},
	})

	decide := func(body string) (code int, out struct {
		Allow  bool   `json:"allow"`
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/policy/decide", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		json.NewDecoder(w.Body).Decode(&out)
		return w.Code, out
	}

	code, out := decide(`{"sandbox":"sb-decide","host":"api.example.com","port":443}`)
	if code != http.StatusOK || !out.Allow {
		t.Fatalf("allow-listed host: code=%d allow=%v reason=%q", code, out.Allow, out.Reason)
	}

	code, out = decide(`{"sandbox":"sb-decide","host":"example.org","port":443}`)
	if code != http.StatusOK || out.Allow {
		t.Fatalf("unlisted host must be refused in-band: code=%d allow=%v", code, out.Allow)
	}
	if out.Reason == "" {
		t.Error("a refusal must carry a reason the model can read")
	}

	// A sandbox with no policy denies — and says how to add one.
	code, out = decide(`{"sandbox":"sb-nopolicy","host":"example.org","port":443}`)
	if code != http.StatusOK || out.Allow {
		t.Fatalf("no policy must deny in-band: code=%d allow=%v", code, out.Allow)
	}

	// Unknown sandbox: still a verdict, not an HTTP error.
	code, out = decide(`{"sandbox":"nobody","host":"example.org","port":443}`)
	if code != http.StatusOK || out.Allow {
		t.Fatalf("unknown sandbox must deny in-band: code=%d allow=%v", code, out.Allow)
	}

	// Malformed requests are errors.
	if code, _ := decide(`{"sandbox":"sb-decide"}`); code != http.StatusBadRequest {
		t.Errorf("missing host/port: code=%d, want 400", code)
	}
}

func TestDecideForSandbox_InvalidPolicyDenies(t *testing.T) {
	sb := &podpkg.SandboxInfo{
		Name:    "sb-bad",
		Network: &egress.Policy{AllowedDomains: []string{"http://not-an-entry"}},
	}
	v := decideForSandbox(sb, true, "example.com", 443)
	if v.Allow {
		t.Fatalf("an unparsable stored policy must deny: %+v", v)
	}
	if !strings.Contains(v.Reason, "invalid") {
		t.Errorf("the reason should say the policy is broken: %q", v.Reason)
	}
}

func TestSplitCapabilities(t *testing.T) {
	cases := map[string][]string{
		"":                  nil,
		"egress":            {"egress"},
		" egress , sandbox": {"egress", "sandbox"},
		",,":                nil,
	}
	for in, want := range cases {
		got := splitCapabilities(in)
		if len(got) != len(want) {
			t.Fatalf("%q → %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q → %v, want %v", in, got, want)
			}
		}
	}
}

// A hosted build issues its own API keys. They must work on /api/v1 *and* on
// /v2/sandboxes — a key that authenticates on one surface and 401s on the other
// is the kind of defect that only shows up when a customer uses the E2B SDK.
func TestE2BGateway_UsesConfiguredAPIKeyResolver(t *testing.T) {
	stores := store.NewMemoryStores()
	calls := 0
	resolver := authenticatorFunc(func(r *http.Request) (Identity, error) {
		calls++
		if r.Header.Get("X-Sandrpod-Token") == "" {
			// the gateway hands the presented key over in this header
			return Identity{}, ErrUnauthenticated
		}
		return Identity{Name: "workspace:acme", Role: RoleUser}, nil
	})
	cfg := Config{Token: "platform", E2BAPIKeys: resolver}
	gw := NewE2BGateway("", cfg, stores, tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	req.Header.Set("X-API-KEY", "sk_customer_key")
	gw.ServeHTTP(rec, req)

	if calls == 0 {
		t.Fatal("the configured resolver was never consulted")
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("a key from the deployment's resolver was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// authenticatorFunc adapts a function to the Authenticator interface.
type authenticatorFunc func(*http.Request) (Identity, error)

func (f authenticatorFunc) Authenticate(r *http.Request) (Identity, error) { return f(r) }

// The E2B surface must scope placement the same way the native API does: a key
// that belongs to a tenant creates sandboxes inside that tenant, not wherever
// there happens to be capacity.
func TestE2BGateway_CarriesTenantIntoPlacement(t *testing.T) {
	stores := store.NewMemoryStores()
	resolver := authenticatorFunc(func(r *http.Request) (Identity, error) {
		if r.Header.Get("X-Sandrpod-Token") != "sk_acme" {
			return Identity{}, ErrUnauthenticated
		}
		return Identity{Name: "workspace:acme", Role: RoleUser, Tenant: "acme"}, nil
	})
	cfg := Config{Token: "platform", E2BAPIKeys: resolver}

	var gotTenant string
	sandboxes := &recordingSandboxes{onCreate: func(req e2bcompat.NewSandbox) { gotTenant = req.Tenant }}
	gw := newTestE2BGateway(cfg, stores, sandboxes)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(`{"templateID":"base"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", "sk_acme")
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if gotTenant != "acme" {
		t.Fatalf("tenant at the backend = %q, want %q", gotTenant, "acme")
	}
}

// recordingSandboxes is a minimal E2B backend that records the create request.
type recordingSandboxes struct {
	onCreate func(e2bcompat.NewSandbox)
}

func (r *recordingSandboxes) CreateSandbox(ident string, admin bool, req e2bcompat.NewSandbox) (e2bcompat.SandboxDetail, error) {
	if r.onCreate != nil {
		r.onCreate(req)
	}
	return e2bcompat.SandboxDetail{SandboxID: "sb-1", TemplateID: req.TemplateID}, nil
}

func (r *recordingSandboxes) GetSandbox(string, string) (e2bcompat.SandboxDetail, bool) {
	return e2bcompat.SandboxDetail{}, false
}

func (r *recordingSandboxes) ListSandboxes(string, map[string]string) []e2bcompat.ListedSandbox {
	return nil
}

func (r *recordingSandboxes) KillSandbox(string, string) bool       { return false }
func (r *recordingSandboxes) SetTimeout(string, string, int32) bool { return true }
func (r *recordingSandboxes) PauseSandbox(string, string) bool      { return false }
func (r *recordingSandboxes) ResumeSandbox(string, string, int32) (e2bcompat.SandboxDetail, bool) {
	return e2bcompat.SandboxDetail{}, false
}
func (r *recordingSandboxes) GetMetrics(string, string) ([]e2bcompat.SandboxMetric, bool) {
	return nil, true
}

// newTestE2BGateway wires the real gateway around a recording backend, so the
// test exercises the same authentication and placement plumbing production
// uses.
func newTestE2BGateway(cfg Config, stores podpkg.Stores, backend e2bcompat.SandboxBackend) http.Handler {
	return e2bcompat.Handler(e2bcompat.Config{
		Authz:     (&e2bDeps{cfg: cfg}).authz(),
		Auth:      (&e2bDeps{cfg: cfg}).authenticator(),
		Sandboxes: backend,
	})
}
