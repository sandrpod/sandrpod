// Copyright 2024 SandrPod
// Gateway: one http.Handler that fronts both E2B planes and dispatches by Host.
//
//	api.<domain>                    → control plane
//	<port>-<sandboxID>.<domain>     → envd for that sandbox
//
// Auth: X-API-KEY (control plane) / Authorization: Bearer (envd access token).
// The authenticated key becomes the request "identity" (ctx) and, for envd
// hosts, the sandbox ID is parsed from the Host and stashed on the ctx too.

package e2bcompat

import (
	"context"
	"net/http"
	"regexp"
	"strings"
)

type ctxKey int

const (
	ctxIdent ctxKey = iota
	ctxSandbox
)

// Authenticator validates a presented E2B key and returns the identity string
// the backends use for ownership/quota. Return ("", false) to reject.
type Authenticator func(key string) (identity string, ok bool)

// Config wires the gateway.
type Config struct {
	// Domain is the base domain the SDK is pointed at (E2B_DOMAIN), e.g.
	// "sandrpod.example.com". Requests arrive at api.<domain> and
	// <port>-<sandboxID>.<domain>. When empty, host routing is disabled and
	// everything is treated as control plane (useful for path-based testing).
	Domain    string
	Auth      Authenticator
	Sandboxes SandboxBackend
	Envd      EnvdBackend
}

// Handler builds the E2B-compatible gateway.
func Handler(cfg Config) http.Handler {
	cp := &controlPlane{backend: cfg.Sandboxes}
	ed := &envd{backend: cfg.Envd}

	cpMux := http.NewServeMux()
	cp.routes(cpMux)
	edMux := http.NewServeMux()
	ed.routes(edMux)

	envdHost := envdHostRe(cfg.Domain)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authenticate.
		key := presentedKey(r.Header.Get("X-API-KEY"), r.Header.Get("Authorization"))
		ident, ok := cfg.Auth(key)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized: missing or invalid API key")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxIdent, ident))

		// Route by host: envd sub-host vs control plane.
		if envdHost != nil {
			if m := envdHost.FindStringSubmatch(hostOnly(r.Host)); m != nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxSandbox, m[2]))
				edMux.ServeHTTP(w, r)
				return
			}
		} else if sid := r.Header.Get("X-Sandbox-ID"); sid != "" {
			// Host routing disabled (tests / path mode): allow an explicit
			// sandbox id header so the envd mux still resolves a target.
			r = r.WithContext(context.WithValue(r.Context(), ctxSandbox, sid))
			if isEnvdPath(r.URL.Path) {
				edMux.ServeHTTP(w, r)
				return
			}
		}
		if isEnvdPath(r.URL.Path) {
			edMux.ServeHTTP(w, r)
			return
		}
		cpMux.ServeHTTP(w, r)
	})
}

// envdHostRe builds a regex matching <port>-<sandboxID>.<domain>, capturing
// (port, sandboxID). Returns nil when domain is empty.
func envdHostRe(domain string) *regexp.Regexp {
	if domain == "" {
		return nil
	}
	return regexp.MustCompile(`^(\d+)-([a-zA-Z0-9_-]+)\.` + regexp.QuoteMeta(domain) + `$`)
}

func isEnvdPath(p string) bool {
	return strings.HasPrefix(p, "/filesystem.") ||
		strings.HasPrefix(p, "/process.") ||
		p == "/files"
}

func hostOnly(host string) string {
	if h, _, ok := strings.Cut(host, ":"); ok {
		return h
	}
	return host
}

// identOf returns the authenticated identity stashed by the gateway.
func identOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxIdent).(string); ok {
		return v
	}
	return ""
}

// sandboxOf returns the sandbox ID for an envd request. It prefers the ctx
// (parsed from Host), falling back to the X-Sandbox-ID header.
func sandboxOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxSandbox).(string); ok && v != "" {
		return v
	}
	return r.Header.Get("X-Sandbox-ID")
}
