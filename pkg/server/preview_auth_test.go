// Copyright 2026 SandrPod Contributors
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

func TestPreviewTokenRoundTrip(t *testing.T) {
	secret := []byte("s3cret")
	tok := mintPreviewToken(secret, "box", 6901, time.Hour)
	if !verifyPreviewToken(secret, tok, "box", 6901) {
		t.Fatal("a freshly minted token must verify")
	}
	// Bound to sandbox and port: a token for one preview must not open another.
	if verifyPreviewToken(secret, tok, "other", 6901) {
		t.Fatal("token verified for a different sandbox")
	}
	if verifyPreviewToken(secret, tok, "box", 9222) {
		t.Fatal("token verified for a different port")
	}
	// Tampering and expiry.
	if verifyPreviewToken([]byte("other-secret"), tok, "box", 6901) {
		t.Fatal("token verified under a different secret")
	}
	expired := mintPreviewToken(secret, "box", 6901, -time.Minute)
	if verifyPreviewToken(secret, expired, "box", 6901) {
		t.Fatal("expired token verified")
	}
}

func TestPortProxyRequiresCredential(t *testing.T) {
	d := e2bDeps{cfg: Config{Token: "platform-token"}, sandboxes: nil}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if d.previewAuthorized(rec, r, "box", 6901) {
		t.Fatal("an unauthenticated preview request was allowed")
	}

	// A valid signed token passes and is persisted as a cookie for the page's
	// subresources and websocket.
	secret := d.cfg.previewSecret()
	tok := mintPreviewToken(secret, "box", 6901, time.Hour)
	r2 := httptest.NewRequest(http.MethodGet, "/?token="+tok, nil)
	rec2 := httptest.NewRecorder()
	if !d.previewAuthorized(rec2, r2, "box", 6901) {
		t.Fatal("a signed token was refused")
	}
	if !strings.Contains(rec2.Header().Get("Set-Cookie"), PreviewCookie) {
		t.Fatalf("expected the preview cookie to be set, got %q", rec2.Header().Get("Set-Cookie"))
	}

	// A platform API key is equally acceptable: the E2B SDK presents one and
	// must keep working.
	r3 := httptest.NewRequest(http.MethodGet, "/", nil)
	r3.Header.Set("Authorization", "Bearer platform-token")
	if !d.previewAuthorized(httptest.NewRecorder(), r3, "box", 6901) {
		t.Fatal("a valid platform token was refused")
	}
}

func TestPreviewURLEndpointShape(t *testing.T) {
	cfg := Config{Token: "t", E2BDomain: "sandbox.example.com"}
	u, err := cfg.previewTokenEndpoint("box", 6901)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://6901-box.sandbox.example.com/?token=") {
		t.Fatalf("unexpected preview URL %q", u)
	}
}

// previewSandboxStore is the lookup handlePreviewURL needs.
type previewSandboxStore map[string]*podpkg.SandboxInfo

func (s previewSandboxStore) Get(name string) (*podpkg.SandboxInfo, bool) {
	sb, ok := s[name]
	return sb, ok
}

func (s previewSandboxStore) List() []*podpkg.SandboxInfo {
	out := make([]*podpkg.SandboxInfo, 0, len(s))
	for _, sb := range s {
		out = append(out, sb)
	}
	return out
}

func (s previewSandboxStore) GetByName(owner, name string) (*podpkg.SandboxInfo, bool) {
	for _, sb := range s {
		if sb.Owner == owner && sb.DisplayNameOf() == name {
			return sb, true
		}
	}
	return nil, false
}

// A signed preview URL is a bearer credential for a desktop (KasmVNC) and a
// debugger (Chrome CDP). Authentication alone must not be enough to mint one for
// somebody else's sandbox.
func TestPreviewURL_IsScopedToTheOwner(t *testing.T) {
	cfg := Config{Token: "platform-token", E2BDomain: "sandbox.example.com"}
	store := previewSandboxStore{
		"box": {Name: "box", Owner: "workspace:alice"},
	}

	call := func(ident Identity) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/box/preview/6901", nil)
		r = r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, ident))
		w := httptest.NewRecorder()
		handlePreviewURL(w, r, cfg, store)
		return w
	}

	if code := call(Identity{Name: "workspace:bob", Role: RoleUser}).Code; code != http.StatusNotFound {
		t.Fatalf("another tenant's preview URL: got %d, want 404", code)
	}
	if code := call(Identity{}).Code; code != http.StatusNotFound {
		t.Fatalf("anonymous preview URL: got %d, want 404", code)
	}

	w := call(Identity{Name: "workspace:alice", Role: RoleUser})
	if w.Code != http.StatusOK {
		t.Fatalf("owner preview URL: got %d, want 200", w.Code)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.URL, "6901-box.sandbox.example.com") || !strings.Contains(out.URL, "token=") {
		t.Fatalf("owner got %q, want a signed URL for the sandbox", out.URL)
	}
	if code := call(Identity{Name: "admin", Role: RoleAdmin}).Code; code != http.StatusOK {
		t.Fatalf("admin preview URL: got %d, want 200", code)
	}
}

// Two workspaces can both have a sandbox called "mysandbox"; each preview URL
// must be minted for that workspace's routing key, never the other's.
func TestPreviewURL_TenantNamesResolveToOwnRoutingKey(t *testing.T) {
	cfg := Config{Token: "platform-token", E2BDomain: "sandbox.example.com"}
	store := previewSandboxStore{
		"sb_aaaaaaaaaaaaaaaa": {Name: "sb_aaaaaaaaaaaaaaaa", DisplayName: "mysandbox", Owner: "workspace:alice"},
		"sb_bbbbbbbbbbbbbbbb": {Name: "sb_bbbbbbbbbbbbbbbb", DisplayName: "mysandbox", Owner: "workspace:bob"},
	}
	call := func(ident Identity) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/mysandbox/preview/6901", nil)
		r = r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, ident))
		w := httptest.NewRecorder()
		handlePreviewURL(w, r, cfg, store)
		return w
	}

	for _, tc := range []struct {
		ident Identity
		key   string
	}{
		{Identity{Name: "workspace:alice", Role: RoleUser}, "sb_aaaaaaaaaaaaaaaa"},
		{Identity{Name: "workspace:bob", Role: RoleUser}, "sb_bbbbbbbbbbbbbbbb"},
	} {
		w := call(tc.ident)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.ident.Name, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, tc.key) {
			t.Fatalf("%s got %s, want its own routing key %s", tc.ident.Name, body, tc.key)
		}
	}
	if w := call(Identity{}); w.Code != http.StatusNotFound {
		t.Fatalf("anonymous got %d, want 404", w.Code)
	}
	// An admin cannot guess between two identical display names; a routing key
	// is unambiguous.
	if w := call(Identity{Name: "admin", Role: RoleAdmin}); w.Code != http.StatusNotFound {
		t.Fatalf("ambiguous admin reference got %d, want 404", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sb_aaaaaaaaaaaaaaaa/preview/6901", nil)
	r = r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, Identity{Name: "workspace:bob", Role: RoleUser}))
	w := httptest.NewRecorder()
	handlePreviewURL(w, r, cfg, store)
	if w.Code != http.StatusNotFound {
		t.Fatalf("bob opened alice's routing key: %d", w.Code)
	}
}
