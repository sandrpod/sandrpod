package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

// Revoking every DB-issued key must not reopen a running server. Before the
// index latched, an empty index made authDisabled true and every request ran
// as an anonymous admin.
func TestAuthStaysOnAfterLastKeyRemoved(t *testing.T) {
	keys := newAPIKeyIndex()
	keys.put("h", Identity{Name: "c", Role: RoleUser})
	keys.remove("h")
	cfg := Config{Keys: keys}
	if cfg.authDisabled() {
		t.Fatal("removing the last key re-opened the server")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	if _, err := cfg.Authenticate(req); err == nil {
		t.Fatal("anonymous request authenticated after the last key was revoked")
	}

	// A periodic multi-instance reload that finds no rows keeps it on too.
	reloaded := newAPIKeyIndex()
	reloaded.replace([]*podpkg.APIToken{{Hash: "h", Name: "c", Role: RoleUser}})
	reloaded.replace(nil)
	if (Config{Keys: reloaded}).authDisabled() {
		t.Fatal("an empty reload re-opened the server")
	}

	// Never having held a key is still the legacy open mode.
	if !(Config{Keys: newAPIKeyIndex()}).authDisabled() {
		t.Fatal("an index that never held a key must leave auth disabled")
	}
}

// DELETE of the only remaining key is refused (409) when no static credential
// exists, because the next restart would otherwise come up open.
func TestTokenDeleteRefusesLastKey(t *testing.T) {
	repo := store.NewMemTokenRepo()
	for _, tok := range []*podpkg.APIToken{
		{Hash: "h1", Prefix: "e2b_aaaa", Name: "a", Role: RoleAdmin},
		{Hash: "h2", Prefix: "e2b_bbbb", Name: "b", Role: RoleUser},
	} {
		if err := repo.Create(tok); err != nil {
			t.Fatal(err)
		}
	}
	keys := newAPIKeyIndex()
	toks, _ := repo.List()
	keys.load(toks)
	cfg := Config{Keys: keys}
	// Config is captured by value, so the handler is rebuilt per call.
	del := func(prefix string) int { return handleTokenDeleteCode(cfg, repo, prefix) }
	if code := del("e2b_aaaa"); code != http.StatusNoContent {
		t.Fatalf("revoking one of two keys: status %d, want 204", code)
	}
	if code := del("e2b_bbbb"); code != http.StatusConflict {
		t.Fatalf("revoking the last key: status %d, want 409", code)
	}
	if left, _ := repo.List(); len(left) != 1 {
		t.Fatalf("last key was deleted despite the refusal: %d left", len(left))
	}

	// With a static token configured the last DB key may go.
	cfg.Token = "static"
	if code := del("e2b_bbbb"); code != http.StatusNoContent {
		t.Fatalf("revoking the last key with -token set: status %d, want 204", code)
	}
	// An unknown prefix still answers 404, not 409.
	cfg.Token = ""
	if code := handleTokenDeleteCode(cfg, repo, "e2b_none"); code != http.StatusNotFound {
		t.Fatalf("unknown prefix: status %d, want 404", code)
	}
}

func handleTokenDeleteCode(cfg Config, repo podpkg.APITokenRepository, prefix string) int {
	rec := httptest.NewRecorder()
	handleTokenDelete(cfg, repo)(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/"+prefix, nil))
	return rec.Code
}
