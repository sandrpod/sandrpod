package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// TestResolveToken_DBIssuedKey locks in that a DB-issued key (matched by hash
// via the in-memory index) authenticates, alongside the legacy admin token, and
// that revocation (index removal) takes effect immediately.
func TestResolveToken_DBIssuedKey(t *testing.T) {
	raw := "e2b_" + strings.Repeat("a", 40)
	idx := newAPIKeyIndex()
	idx.put(hashKey(raw), Identity{Name: "client-x", Role: RoleUser})
	cfg := Config{Token: "admin-tok", Keys: idx}

	if id, ok := resolveToken(cfg, raw); !ok || id.Name != "client-x" || id.Role != RoleUser {
		t.Errorf("db key: got (%+v, %v), want client-x/user/true", id, ok)
	}
	if id, ok := resolveToken(cfg, "admin-tok"); !ok || !id.IsAdmin() {
		t.Errorf("admin token regressed: got (%+v, %v)", id, ok)
	}
	if _, ok := resolveToken(cfg, "e2b_"+strings.Repeat("b", 40)); ok {
		t.Error("unknown key: want reject")
	}
	// Revoke → immediately stops resolving.
	idx.remove(hashKey(raw))
	if _, ok := resolveToken(cfg, raw); ok {
		t.Error("revoked key still resolves")
	}
}

func TestLoadTokensFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("bare array with default role", func(t *testing.T) {
		toks, err := loadTokensFile(write("a.json", `[{"name":"alice","token":"tok-a"},{"name":"ops","token":"tok-o","role":"admin"}]`))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(toks) != 2 || toks[0].Role != RoleUser || toks[1].Role != RoleAdmin {
			t.Errorf("unexpected tokens: %+v", toks)
		}
	})
	t.Run("wrapped object", func(t *testing.T) {
		toks, err := loadTokensFile(write("b.json", `{"tokens":[{"name":"x","token":"t"}]}`))
		if err != nil || len(toks) != 1 {
			t.Fatalf("load: %v %+v", err, toks)
		}
	})
	t.Run("duplicate name rejected", func(t *testing.T) {
		if _, err := loadTokensFile(write("c.json", `[{"name":"a","token":"1"},{"name":"a","token":"2"}]`)); err == nil {
			t.Error("expected duplicate-name error")
		}
	})
	t.Run("bad role rejected", func(t *testing.T) {
		if _, err := loadTokensFile(write("d.json", `[{"name":"a","token":"1","role":"root"}]`)); err == nil {
			t.Error("expected bad-role error")
		}
	})
	t.Run("missing token rejected", func(t *testing.T) {
		if _, err := loadTokensFile(write("e.json", `[{"name":"a"}]`)); err == nil {
			t.Error("expected missing-token error")
		}
	})
}

func TestResolveToken(t *testing.T) {
	cfg := Config{
		Token: "legacy-secret",
		Tokens: []NamedToken{
			{Name: "alice", Token: "tok-alice", Role: RoleUser},
			{Name: "ops", Token: "tok-ops", Role: RoleAdmin},
		},
	}
	if id, ok := resolveToken(cfg, "legacy-secret"); !ok || !id.IsAdmin() || id.Name != "admin" {
		t.Errorf("legacy token must resolve as admin, got %+v %v", id, ok)
	}
	if id, ok := resolveToken(cfg, "tok-alice"); !ok || id.IsAdmin() || id.Name != "alice" {
		t.Errorf("alice must resolve as user, got %+v %v", id, ok)
	}
	if id, ok := resolveToken(cfg, "tok-ops"); !ok || !id.IsAdmin() {
		t.Errorf("ops must resolve as admin, got %+v %v", id, ok)
	}
	if _, ok := resolveToken(cfg, "nope"); ok {
		t.Error("unknown token must not resolve")
	}
}

func TestCanAccessSandbox(t *testing.T) {
	admin := Identity{Name: "ops", Role: RoleAdmin}
	alice := Identity{Name: "alice", Role: RoleUser}
	bob := Identity{Name: "bob", Role: RoleUser}

	owned := &podpkg.SandboxInfo{Owner: "alice"}
	legacy := &podpkg.SandboxInfo{} // pre-upgrade record, no owner

	if !canAccessSandbox(admin, owned) {
		t.Error("admin must access everything")
	}
	if !canAccessSandbox(alice, owned) {
		t.Error("owner must access their sandbox")
	}
	if canAccessSandbox(bob, owned) {
		t.Error("other users must not access it")
	}
	if !canAccessSandbox(bob, legacy) {
		t.Error("ownerless legacy records stay visible to all")
	}
}

// TestAuthDisabled asserts that any configured credential — including a single
// DB-issued key — turns auth on, so an operator can't leave the server open
// while believing issued keys secure it.
func TestAuthDisabled(t *testing.T) {
	if !(Config{}).authDisabled() {
		t.Error("no credentials: want auth disabled")
	}
	if (Config{Token: "t"}).authDisabled() {
		t.Error("-token set: want auth enabled")
	}
	if (Config{Tokens: []NamedToken{{Name: "a", Token: "x"}}}).authDisabled() {
		t.Error("tokens-file set: want auth enabled")
	}
	empty := Config{Keys: newAPIKeyIndex()}
	if !empty.authDisabled() {
		t.Error("empty key index: want auth disabled")
	}
	withKey := newAPIKeyIndex()
	withKey.put("h", Identity{Name: "c", Role: RoleUser})
	if (Config{Keys: withKey}).authDisabled() {
		t.Error("DB key issued: want auth enabled")
	}
}

// The zero Identity must not be an administrator. A request that reaches a
// handler without the middleware having set an identity is a bug — a new route
// that forgot its middleware — and answering it as an admin is how a missing
// middleware becomes a vulnerability. This pins the fail-closed default.
func TestIdentityFrom_UnsetIsNotAdmin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	id := IdentityFrom(req)
	if id.IsAdmin() {
		t.Fatal("an unset identity must not be an admin")
	}
	if id.Name != "" {
		t.Fatalf("an unset identity has no name, got %q", id.Name)
	}
}

// A custom Authenticator replaces the built-in scheme wholesale, and its
// refusal reasons map to distinct status codes: a credential that is merely
// absent is not the same answer as one that is not allowed, or a caller that is
// being throttled.
func TestAuthMiddleware_MapsAuthenticatorRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unauthenticated", ErrUnauthenticated, http.StatusUnauthorized},
		{"forbidden", ErrForbidden, http.StatusForbidden},
		{"rate limited", ErrRateLimited, http.StatusTooManyRequests},
		{"unknown", errors.New("backend down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Token:         "t", // keep the legacy open-server path off
				Authenticator: denyingAuthenticator{err: tc.err},
			}
			h := New(cfg, store.NewMemoryStores(), tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
			req.Header.Set("X-Sandrpod-Token", "t")
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// An Authenticator that always refuses, for the mapping test.
type denyingAuthenticator struct{ err error }

func (d denyingAuthenticator) Authenticate(*http.Request) (Identity, error) { return Identity{}, d.err }
