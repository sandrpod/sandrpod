// Copyright 2026 SandrPod Contributors
// Named-token authentication and sandbox ownership.
//
// Backward compatible by design: the legacy single -token keeps working (as an
// implicit admin token named "admin"), and with no tokens configured at all,
// auth stays disabled exactly as before. A tokens file adds individually
// revocable named tokens; user-role tokens only see and manage the sandboxes
// they created.

package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// Token roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
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
			t.Role = RoleUser
		}
		if t.Role != RoleAdmin && t.Role != RoleUser {
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

// tokenRegistry holds the live named-token set, swappable at runtime so the
// tokens file can be hot-reloaded (revocations apply without a restart).
type tokenRegistry struct {
	mu   sync.RWMutex
	list []NamedToken
}

func (tr *tokenRegistry) get() []NamedToken {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	return tr.list
}

func (tr *tokenRegistry) set(list []NamedToken) {
	tr.mu.Lock()
	tr.list = list
	tr.mu.Unlock()
}

// watchTokensFile polls the tokens file every 10 s and swaps the registry on
// change. A parse error keeps the previous set (fail-safe: never lock everyone
// out because of a half-written edit).
func watchTokensFile(ctx context.Context, path string, tr *tokenRegistry) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var lastMod time.Time
	if st, err := os.Stat(path); err == nil {
		lastMod = st.ModTime()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		st, err := os.Stat(path)
		if err != nil || !st.ModTime().After(lastMod) {
			continue
		}
		lastMod = st.ModTime()
		toks, err := loadTokensFile(path)
		if err != nil {
			log.Printf("tokens file %s changed but failed to parse (keeping previous set): %v", path, err)
			continue
		}
		tr.set(toks)
		log.Printf("tokens file reloaded: %d token(s)", len(toks))
	}
}

// hashKey returns the hex SHA-256 of a raw API key. Persisted tokens store this
// hash (never the raw key), and the auth index is keyed by it.
func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// apiKeyIndex is the in-memory hot-path lookup for DB-issued API tokens:
// sha256(key) -> Identity. It is loaded from the token store at startup and
// updated in lockstep as tokens are issued/revoked, so authentication never
// touches the database. The DB remains the durable source of truth.
type apiKeyIndex struct {
	mu sync.RWMutex
	m  map[string]Identity
}

func newAPIKeyIndex() *apiKeyIndex { return &apiKeyIndex{m: map[string]Identity{}} }

func (x *apiKeyIndex) get(hash string) (Identity, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	id, ok := x.m[hash]
	return id, ok
}

func (x *apiKeyIndex) put(hash string, id Identity) {
	x.mu.Lock()
	x.m[hash] = id
	x.mu.Unlock()
}

func (x *apiKeyIndex) remove(hash string) {
	x.mu.Lock()
	delete(x.m, hash)
	x.mu.Unlock()
}

func (x *apiKeyIndex) len() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.m)
}

// load bulk-populates the index from persisted tokens (startup).
func (x *apiKeyIndex) load(toks []*podpkg.APIToken) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, t := range toks {
		x.m[t.Hash] = Identity{Name: t.Name, Role: t.Role}
	}
}

// replace swaps the whole index atomically. Used by the periodic multi-instance
// reload so tokens issued OR revoked on peer instances converge here.
func (x *apiKeyIndex) replace(toks []*podpkg.APIToken) {
	m := make(map[string]Identity, len(toks))
	for _, t := range toks {
		m[t.Hash] = Identity{Name: t.Name, Role: t.Role}
	}
	x.mu.Lock()
	x.m = m
	x.mu.Unlock()
}

// Identity is who a request authenticated as.
type Identity struct {
	Name string
	Role string
}

func (id Identity) IsAdmin() bool { return id.Role == RoleAdmin }

// Authenticator decides who made a request. The default implementation is the
// built-in token scheme (Config.Token, the tokens file, DB-issued keys); a
// different program can supply registration, sessions or OIDC instead.
//
// Returning an error refuses the request. The sentinels below map to status
// codes, so an implementation can distinguish "no credential" from "credential
// for someone else" from "too many requests" instead of collapsing them into
// one 401.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// Refusal reasons an Authenticator can return. Anything else is treated as
// ErrUnauthenticated plus a log line, because an unrecognised error must not
// become a 200.
var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrRateLimited     = errors.New("rate limited")
)

// Authenticate implements Authenticator with the built-in token scheme.
func (cfg Config) Authenticate(r *http.Request) (Identity, error) {
	if cfg.authDisabled() {
		// Legacy: no credential configured means the server is open, and every
		// request is an anonymous admin. Kept for upgrade smoothness; a build
		// that must never be open supplies its own Authenticator.
		return Identity{Name: "", Role: RoleAdmin}, nil
	}
	if id, ok := resolveRequest(cfg, r); ok {
		// Per-identity rate limit for user tokens (admins/poders exempt).
		if apiRateLimit != nil && !id.IsAdmin() && !apiRateLimit.allow(id.Name) {
			return Identity{}, ErrRateLimited
		}
		return id, nil
	}
	return Identity{}, ErrUnauthenticated
}

type identityCtxKey struct{}

// IdentityFrom returns the request's authenticated Identity.
//
// The zero value here is deliberately NOT an admin: a request that reaches a
// handler without the middleware having set an identity is a bug (or a new
// route that forgot its middleware), and answering it as an administrator is
// how such a bug becomes a vulnerability. IsAdmin() is false for the zero
// value, so the failure is "no access" rather than "all access".
func IdentityFrom(r *http.Request) Identity {
	if id, ok := r.Context().Value(identityCtxKey{}).(Identity); ok {
		return id
	}
	return Identity{}
}

// resolveToken matches a presented credential against the configured tokens
// (constant-time per candidate). The legacy single token authenticates as an
// admin named "admin". Named tokens come from the static cfg.Tokens plus the
// hot-reloadable registry, when configured.
// authDisabled reports whether no credential of any kind is configured, in
// which case every request runs as an anonymous admin (legacy dev behaviour).
// Issued DB tokens count — minting a key turns auth on — so an operator can't
// accidentally leave the server open while believing their keys secure it.
func (cfg Config) authDisabled() bool {
	return cfg.Token == "" &&
		len(cfg.Tokens) == 0 &&
		(cfg.Registry == nil || len(cfg.Registry.get()) == 0) &&
		(cfg.Keys == nil || cfg.Keys.len() == 0)
}

func resolveToken(cfg Config, presented string) (Identity, bool) {
	if cfg.Token != "" &&
		subtle.ConstantTimeCompare([]byte(presented), []byte(cfg.Token)) == 1 {
		return Identity{Name: "admin", Role: RoleAdmin}, true
	}
	candidates := cfg.Tokens
	if cfg.Registry != nil {
		candidates = append(candidates, cfg.Registry.get()...)
	}
	for _, t := range candidates {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(t.Token)) == 1 {
			return Identity{Name: t.Name, Role: t.Role}, true
		}
	}
	// DB-issued tokens: matched by hash via the in-memory index. Preimage
	// resistance means a hash-map lookup leaks nothing usable, so a plain lookup
	// (not constant-time) is safe here.
	if cfg.Keys != nil {
		h := hashKey(presented)
		if id, ok := cfg.Keys.get(h); ok {
			return id, true
		}
		// Multi-instance fallback: a token issued on a peer instance isn't in
		// this instance's index yet. Look it up in the shared store and cache it.
		if cfg.NodeURL != "" && cfg.TokenStore != nil {
			if t, ok := cfg.TokenStore.FindByHash(h); ok {
				id := Identity{Name: t.Name, Role: t.Role}
				cfg.Keys.put(h, id)
				return id, true
			}
		}
	}
	return Identity{}, false
}

// resolveRequest tries each supported credential header in turn: a wrong
// X-Sandrpod-Token still falls back to the Authorization Bearer (callers may
// carry an unrelated Bearer, e.g. an MCP token, alongside the platform token).
func resolveRequest(cfg Config, r *http.Request) (Identity, bool) {
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
	return Identity{}, false
}

// WithIdentity stores the Identity on the request context.
func WithIdentity(r *http.Request, id Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, id))
}

// canAccessSandbox reports whether an Identity may see/manage a sandbox.
// Admins see everything; ownerless records (created before multi-token auth,
// or with auth disabled) stay visible to everyone for upgrade smoothness.
func canAccessSandbox(id Identity, sb *podpkg.SandboxInfo) bool {
	return id.IsAdmin() || sb.Owner == "" || sb.Owner == id.Name
}

// canAccessJob is the job-record equivalent of canAccessSandbox.
func canAccessJob(id Identity, j *podpkg.Job) bool {
	return id.IsAdmin() || j.Owner == "" || j.Owner == id.Name
}
