// API-token issuance: admin endpoints that mint, list, and revoke API tokens
// persisted in the store. Issued keys use E2B's canonical e2b_<hex> shape so
// they double as a drop-in E2B_API_KEY (no E2B_VALIDATE_API_KEY=false needed)
// and authenticate identically on the native API. Only the SHA-256 hash is
// persisted — the raw key is shown once at creation and never retrievable.

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sandrpod/sandrpod/pkg/e2bcompat"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

const lastKeyRefusal = "refusing to revoke the last API key: with no key left and no -token / -tokens-file configured, " +
	"the server would run with authentication disabled after its next restart. " +
	"Issue a replacement key first (or configure SANDRPOD_TOKEN), then revoke this one."

// tokenRevokeMu serialises the check-then-delete fallback for repositories that
// do not implement LastTokenGuard. Repositories that do are atomic on their own.
var tokenRevokeMu sync.Mutex

// deleteUnlessLast revokes a prefix unless it is the table's only content. It
// is the fallback for a repository without LastTokenGuard; refused reports the
// refusal separately from "nothing matched", which is a 404.
func deleteUnlessLast(tokens podpkg.APITokenRepository, prefix string) (removed []string, refused bool, err error) {
	list, err := tokens.List()
	if err != nil {
		return nil, false, err
	}
	left := 0
	for _, t := range list {
		if t.Prefix != prefix {
			left++
		}
	}
	if left == 0 && len(list) > 0 {
		return nil, true, nil
	}
	removed, err = tokens.DeleteByPrefix(prefix)
	return removed, false, err
}

func writeTokenJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleTokens serves GET (list) and POST (issue) on /api/v1/tokens.
func handleTokens(cfg Config, tokens podpkg.APITokenRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			http.Error(w, "token store not configured (start the server with a persistent -db backend: sqlite:... or postgres://...)", http.StatusNotImplemented)
			return
		}
		switch r.Method {
		case http.MethodGet:
			list, err := tokens.List()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if list == nil {
				list = []*podpkg.APIToken{}
			}
			writeTokenJSON(w, http.StatusOK, map[string]any{"tokens": list})

		case http.MethodPost:
			var req struct {
				Name string `json:"name"`
				Role string `json:"role"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			req.Name = strings.TrimSpace(req.Name)
			if req.Name == "" {
				http.Error(w, "name is required", http.StatusBadRequest)
				return
			}
			role := RoleUser
			switch req.Role {
			case "", RoleUser:
				role = RoleUser
			case RoleAdmin:
				role = RoleAdmin
			default:
				http.Error(w, "role must be admin or user", http.StatusBadRequest)
				return
			}
			raw, err := e2bcompat.GenerateAPIKey()
			if err != nil {
				http.Error(w, "failed to generate key", http.StatusInternalServerError)
				return
			}
			tok := &podpkg.APIToken{
				Name:      req.Name,
				Prefix:    raw[:16], // "e2b_" + first 12 hex — a stable display/revoke id
				Hash:      hashKey(raw),
				Role:      role,
				CreatedAt: time.Now(),
			}
			if err := tokens.Create(tok); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if cfg.Keys != nil {
				cfg.Keys.put(tok.Hash, Identity{Name: tok.Name, Role: tok.Role})
			}
			if cfg.NotifyTokenChange != nil {
				cfg.NotifyTokenChange() // wake peer instances to reload (multi-instance)
			}
			// The raw key is returned exactly once; only its hash is stored.
			writeTokenJSON(w, http.StatusCreated, map[string]any{
				"name":       tok.Name,
				"role":       tok.Role,
				"prefix":     tok.Prefix,
				"key":        raw,
				"created_at": tok.CreatedAt,
			})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// handleTokenDelete revokes a token by its display prefix:
// DELETE /api/v1/tokens/{prefix}.
func handleTokenDelete(cfg Config, tokens podpkg.APITokenRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			http.Error(w, "token store not configured (start the server with a persistent -db backend: sqlite:... or postgres://...)", http.StatusNotImplemented)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		prefix := strings.TrimPrefix(r.URL.Path, "/api/v1/tokens/")
		if prefix == "" || strings.Contains(prefix, "/") {
			http.Error(w, "usage: DELETE /api/v1/tokens/{prefix}", http.StatusBadRequest)
			return
		}
		// Refuse to revoke the last credential. The running server stays closed
		// either way (the key index latches), but with no key left and no static
		// token the next restart would come up as an anonymous-admin endpoint.
		var removed []string
		if !cfg.hasStaticCredential() {
			if guard, ok := tokens.(podpkg.LastTokenGuard); ok {
				// The repository decides atomically with the delete, so two
				// concurrent revocations cannot both see a survivor.
				var err error
				removed, err = guard.DeleteByPrefixIfNotLast(prefix)
				if errors.Is(err, podpkg.ErrLastToken) {
					http.Error(w, lastKeyRefusal, http.StatusConflict)
					return
				}
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			} else {
				// A repository without the guard: serialise in this process and
				// accept that a peer instance could still race us.
				tokenRevokeMu.Lock()
				var refused bool
				var err error
				removed, refused, err = deleteUnlessLast(tokens, prefix)
				tokenRevokeMu.Unlock()
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if refused {
					http.Error(w, lastKeyRefusal, http.StatusConflict)
					return
				}
			}
		} else {
			var err error
			removed, err = tokens.DeleteByPrefix(prefix)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if len(removed) == 0 {
			http.Error(w, "no token with that prefix", http.StatusNotFound)
			return
		}
		if cfg.Keys != nil {
			for _, h := range removed {
				cfg.Keys.remove(h)
			}
		}
		if cfg.NotifyTokenChange != nil {
			cfg.NotifyTokenChange() // wake peer instances to drop the revoked key now
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
