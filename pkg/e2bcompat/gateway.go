// Copyright 2024 SandrPod
// Gateway: one http.Handler that fronts both E2B planes and dispatches by Host.
//
//	api.<domain>                    → control plane
//	<port>-<sandboxID>.<domain>     → envd for that sandbox (envd/code ports)
//	<port>-<sandboxID>.<domain>     → generic in-sandbox service (PortProxy)
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
	// Forwarder, when set, is given each envd/code request once its target
	// sandbox is known. In multi-instance load mode the sandbox's tunnel may
	// terminate on a peer node; the forwarder reverse-proxies the request there
	// and returns true (request handled). Returning false means "serve locally"
	// (the tunnel is here, or there is no peer owner). Control-plane requests
	// never reach it — they read the shared store and are served on any node.
	Forwarder func(w http.ResponseWriter, r *http.Request, sandbox string) bool
	// PortProxy, when set, handles a generic host-port request —
	// <port>-<sandboxID>.<domain>/<path> where the port is neither envd nor the
	// code-interpreter port and the path is not an envd/code path. This is how
	// E2B's in-sandbox services are reached (e.g. the MCP gateway on :50005, a
	// user's dev server, etc.): the request is proxied through the sandbox's
	// tunnel to the toolbox's /proxy/<port>/ mount, which reverse-proxies to
	// 127.0.0.1:<port> inside the sandbox. Returns true when handled. Returning
	// false (or leaving it nil) falls through to the control plane.
	PortProxy func(w http.ResponseWriter, r *http.Request, sandbox string, port int) bool
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
			// E2B's code-interpreter talks to the local Jupyter kernel
			// unauthenticated under E2B_DEBUG=true — its /execute + /contexts
			// calls carry no X-API-KEY/token at all. In path/debug mode (no
			// vanity domain) tolerate that for those code paths so the debug
			// port is usable against an auth-enabled server; the single-sandbox
			// resolver still scopes execution. Domain (production) mode, where
			// the SDK does send the envd token, stays strict.
			codePath := r.URL.Path == "/execute" || strings.HasPrefix(r.URL.Path, "/contexts")
			if cfg.Domain != "" || key != "" || !codePath {
				writeErr(w, http.StatusUnauthorized, "unauthorized: missing or invalid API key")
				return
			}
			ident = "" // anonymous; the resolver picks the sole sandbox
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxIdent, ident))

		// Resolve the target sandbox and whether this is a code-interpreter
		// request. The SDK sends E2b-Sandbox-Id / E2b-Sandbox-Port headers; we
		// also accept the Host (<port>-<id>.<domain>) and fall back to the
		// single-sandbox resolver (fixed sandbox URL in HTTP debug mode).
		sandbox, isCode := "", false
		hostPort := 0 // >0 when the Host matched <port>-<sandboxID>.<domain>
		if envdHost != nil {
			if m := envdHost.FindStringSubmatch(hostOnly(r.Host)); m != nil {
				sandbox, isCode = m[2], m[1] == strconv.Itoa(codePort)
				hostPort, _ = strconv.Atoi(m[1])
			}
		}
		if sandbox == "" {
			if sid := r.Header.Get("E2b-Sandbox-Id"); sid != "" {
				sandbox = sid
			} else {
				sandbox = r.Header.Get("X-Sandbox-ID")
			}
		}
		// E2B's debug mode uses a placeholder id; resolve it to the real one.
		if sandbox == "debug_sandbox_id" {
			sandbox = ""
		}
		if p := r.Header.Get("E2b-Sandbox-Port"); p == strconv.Itoa(codePort) {
			isCode = true
		}

		isEnvd := isEnvdPath(r.URL.Path)
		isCodePath := r.URL.Path == "/execute" || strings.HasPrefix(r.URL.Path, "/contexts")
		if isEnvd || isCodePath {
			if sandbox == "" && cfg.SandboxResolver != nil {
				sandbox = cfg.SandboxResolver(ident)
			}
			// Multi-instance: if this sandbox's tunnel lives on a peer node, the
			// forwarder reverse-proxies the request there (envd/code need the
			// tunnel; the control plane above does not).
			if cfg.Forwarder != nil && sandbox != "" && cfg.Forwarder(w, r, sandbox) {
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), ctxSandbox, sandbox))
			if isCode || isCodePath {
				ciMux.ServeHTTP(w, r)
				return
			}
			edMux.ServeHTTP(w, r)
			return
		}
		// Generic in-sandbox service reached at <port>-<sandboxID>.<domain>: the
		// Host named a port that is neither envd nor the code interpreter, and the
		// path is not an envd/code path. This is how E2B exposes in-sandbox HTTP
		// services — the MCP gateway on :50005, a user dev server, etc. Proxy it
		// through the tunnel to the toolbox's /proxy/<port>/ mount, which reverse-
		// proxies to 127.0.0.1:<port> inside the sandbox.
		if hostPort > 0 && !isCode && cfg.PortProxy != nil {
			if sandbox == "" && cfg.SandboxResolver != nil {
				sandbox = cfg.SandboxResolver(ident)
			}
			if sandbox != "" {
				// Cross-node: forward to the owner node first if the tunnel is remote.
				if cfg.Forwarder != nil && cfg.Forwarder(w, r, sandbox) {
					return
				}
				if cfg.PortProxy(w, r, sandbox, hostPort) {
					return
				}
			}
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
