// API-token issuance: admin endpoints that mint, list, and revoke API tokens
// persisted in the store. Issued keys use E2B's canonical e2b_<hex> shape so
// they double as a drop-in E2B_API_KEY (no E2B_VALIDATE_API_KEY=false needed)
// and authenticate identically on the native API. Only the SHA-256 hash is
// persisted — the raw key is shown once at creation and never retrievable.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sandrpod/sandrpod/pkg/e2bcompat"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

func writeTokenJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleTokens serves GET (list) and POST (issue) on /api/v1/tokens.
func handleTokens(cfg serverConfig, tokens podpkg.APITokenRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			http.Error(w, "token store not configured (start the server with -db sqlite:...)", http.StatusNotImplemented)
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
			role := roleUser
			switch req.Role {
			case "", roleUser:
				role = roleUser
			case roleAdmin:
				role = roleAdmin
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
				cfg.Keys.put(tok.Hash, identity{Name: tok.Name, Role: tok.Role})
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
func handleTokenDelete(cfg serverConfig, tokens podpkg.APITokenRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			http.Error(w, "token store not configured (start the server with -db sqlite:...)", http.StatusNotImplemented)
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
		removed, err := tokens.DeleteByPrefix(prefix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
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
