package main

import (
	"os"
	"path/filepath"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

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
		if len(toks) != 2 || toks[0].Role != roleUser || toks[1].Role != roleAdmin {
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
	cfg := serverConfig{
		Token: "legacy-secret",
		Tokens: []NamedToken{
			{Name: "alice", Token: "tok-alice", Role: roleUser},
			{Name: "ops", Token: "tok-ops", Role: roleAdmin},
		},
	}
	if id, ok := resolveToken(cfg, "legacy-secret"); !ok || !id.isAdmin() || id.Name != "admin" {
		t.Errorf("legacy token must resolve as admin, got %+v %v", id, ok)
	}
	if id, ok := resolveToken(cfg, "tok-alice"); !ok || id.isAdmin() || id.Name != "alice" {
		t.Errorf("alice must resolve as user, got %+v %v", id, ok)
	}
	if id, ok := resolveToken(cfg, "tok-ops"); !ok || !id.isAdmin() {
		t.Errorf("ops must resolve as admin, got %+v %v", id, ok)
	}
	if _, ok := resolveToken(cfg, "nope"); ok {
		t.Error("unknown token must not resolve")
	}
}

func TestCanAccessSandbox(t *testing.T) {
	admin := identity{Name: "ops", Role: roleAdmin}
	alice := identity{Name: "alice", Role: roleUser}
	bob := identity{Name: "bob", Role: roleUser}

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
