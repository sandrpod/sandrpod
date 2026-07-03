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
	"strconv"
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
	// Code, when set, backs the code-interpreter run_code surface reached at
	// <CodePort>-<sandboxID>.<domain>/execute. CodePort defaults to 49999.
	Code     CodeInterpreter
	CodePort int
	// SandboxResolver resolves the target sandbox for an envd/code request when
	// it can't be derived from the Host (e.g. HTTP debug mode where the SDK is
	// pointed at a fixed E2B_SANDBOX_URL). Given the caller's identity it
	// returns their sandbox ID. Optional; used only as a fallback.
	SandboxResolver func(identity string) string
}

// Handler builds the E2B-compatible gateway.
func Handler(cfg Config) http.Handler {
	cp := &controlPlane{backend: cfg.Sandboxes}
	ed := &envd{backend: cfg.Envd}

	cpMux := http.NewServeMux()
	cp.routes(cpMux)
	edMux := http.NewServeMux()
	ed.routes(edMux)

	codePort := cfg.CodePort
	if codePort == 0 {
		codePort = DefaultCodeInterpreterPort
	}
	ciMux := http.NewServeMux()
	if cfg.Code != nil {
		(&codeInterp{backend: cfg.Code}).routes(ciMux)
	}

	envdHost := envdHostRe(cfg.Domain)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authenticate. The control plane uses X-API-KEY / Authorization; envd
		// uses the X-Access-Token header (the sandbox's envd access token).
		key := presentedKey(r.Header.Get("X-API-KEY"), r.Header.Get("Authorization"))
		if key == "" {
			key = r.Header.Get("X-Access-Token")
		}
		ident, ok := cfg.Auth(key)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized: missing or invalid API key")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxIdent, ident))

		// Resolve the target sandbox and whether this is a code-interpreter
		// request. The SDK sends E2b-Sandbox-Id / E2b-Sandbox-Port headers; we
		// also accept the Host (<port>-<id>.<domain>) and fall back to the
		// single-sandbox resolver (fixed sandbox URL in HTTP debug mode).
		sandbox, isCode := "", false
		if envdHost != nil {
			if m := envdHost.FindStringSubmatch(hostOnly(r.Host)); m != nil {
				sandbox, isCode = m[2], m[1] == strconv.Itoa(codePort)
			}
		}
		if sandbox == "" {
			if sid := r.Header.Get("E2b-Sandbox-Id"); sid != "" {
				sandbox = sid
			} else {
				sandbox = r.Header.Get("X-Sandbox-ID")
			}
		}
		if p := r.Header.Get("E2b-Sandbox-Port"); p == strconv.Itoa(codePort) {
			isCode = true
		}

		isEnvd := isEnvdPath(r.URL.Path)
		isExec := r.URL.Path == "/execute"
		if isEnvd || isExec {
			if sandbox == "" && cfg.SandboxResolver != nil {
				sandbox = cfg.SandboxResolver(ident)
			}
			r = r.WithContext(context.WithValue(r.Context(), ctxSandbox, sandbox))
			if isCode || isExec {
				ciMux.ServeHTTP(w, r)
				return
			}
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
