// Copyright 2024 SandrPod
// Named-token authentication and sandbox ownership.
//
// Backward compatible by design: the legacy single -token keeps working (as an
// implicit admin token named "admin"), and with no tokens configured at all,
// auth stays disabled exactly as before. A tokens file adds individually
// revocable named tokens; user-role tokens only see and manage the sandboxes
// they created.

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// Token roles.
const (
	roleAdmin = "admin"
	roleUser  = "user"
)

// NamedToken is one entry of the -tokens-file.
type NamedToken struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	Role  string `json:"role"` // "admin" | "user" (default "user")
}

// loadTokensFile parses a JSON tokens file: either a bare array of NamedToken
// or {"tokens": [...]}. Names and tokens must be non-empty and unique.
func loadTokensFile(path string) ([]NamedToken, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []NamedToken
	if err := json.Unmarshal(data, &list); err != nil {
		var wrapped struct {
			Tokens []NamedToken `json:"tokens"`
		}
		if err2 := json.Unmarshal(data, &wrapped); err2 != nil {
			return nil, fmt.Errorf("tokens file must be a JSON array or {\"tokens\": [...]}: %w", err)
		}
		list = wrapped.Tokens
	}
	seenName := map[string]bool{}
	seenTok := map[string]bool{}
	for i := range list {
		t := &list[i]
		if t.Name == "" || t.Token == "" {
			return nil, fmt.Errorf("tokens file entry %d: name and token are required", i)
		}
		if t.Role == "" {
			t.Role = roleUser
		}
		if t.Role != roleAdmin && t.Role != roleUser {
			return nil, fmt.Errorf("tokens file entry %q: role must be admin or user", t.Name)
		}
		if seenName[t.Name] || seenTok[t.Token] {
			return nil, fmt.Errorf("tokens file entry %q: duplicate name or token", t.Name)
		}
		seenName[t.Name] = true
		seenTok[t.Token] = true
	}
	return list, nil
}

// identity is who a request authenticated as.
type identity struct {
	Name string
	Role string
}

func (id identity) isAdmin() bool { return id.Role == roleAdmin }

type identityCtxKey struct{}

// identityFrom returns the request's authenticated identity. When auth is
// disabled the middleware stores an anonymous admin, so the zero-value
// fallback here is defensive only.
func identityFrom(r *http.Request) identity {
	if id, ok := r.Context().Value(identityCtxKey{}).(identity); ok {
		return id
	}
	return identity{Name: "", Role: roleAdmin}
}

// resolveToken matches a presented credential against the configured tokens
// (constant-time per candidate). The legacy single token authenticates as an
// admin named "admin".
func resolveToken(cfg serverConfig, presented string) (identity, bool) {
	if cfg.Token != "" &&
		subtle.ConstantTimeCompare([]byte(presented), []byte(cfg.Token)) == 1 {
		return identity{Name: "admin", Role: roleAdmin}, true
	}
	for _, t := range cfg.Tokens {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(t.Token)) == 1 {
			return identity{Name: t.Name, Role: t.Role}, true
		}
	}
	return identity{}, false
}

// resolveRequest tries each supported credential header in turn: a wrong
// X-Sandrpod-Token still falls back to the Authorization Bearer (callers may
// carry an unrelated Bearer, e.g. an MCP token, alongside the platform token).
func resolveRequest(cfg serverConfig, r *http.Request) (identity, bool) {
	if got := r.Header.Get("X-Sandrpod-Token"); got != "" {
		if id, ok := resolveToken(cfg, got); ok {
			return id, true
		}
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if id, ok := resolveToken(cfg, auth[len("Bearer "):]); ok {
			return id, true
		}
	}
	return identity{}, false
}

// withIdentity stores the identity on the request context.
func withIdentity(r *http.Request, id identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, id))
}

// canAccessSandbox reports whether an identity may see/manage a sandbox.
// Admins see everything; ownerless records (created before multi-token auth,
// or with auth disabled) stay visible to everyone for upgrade smoothness.
func canAccessSandbox(id identity, sb *podpkg.SandboxInfo) bool {
	return id.isAdmin() || sb.Owner == "" || sb.Owner == id.Name
}

// canAccessJob is the job-record equivalent of canAccessSandbox.
func canAccessJob(id identity, j *podpkg.Job) bool {
	return id.isAdmin() || j.Owner == "" || j.Owner == id.Name
}
