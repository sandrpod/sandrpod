// Copyright 2026 SandrPod Contributors
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
	// Authorize reports whether the authenticated identity may act on the
	// resolved sandbox. It gates the DATA plane (envd filesystem/process,
	// code-interpreter, and the generic port proxy), which — unlike the
	// control plane — derives its target sandbox from a caller-supplied
	// header/Host and would otherwise let any authenticated caller reach any
	// tenant's sandbox by ID. Return true to allow. When nil the check is
	// skipped (single-tenant / auth-disabled deployments). Called only for a
	// non-empty resolved sandbox, before forwarding or dispatch.
	Authorize func(identity, sandbox string) bool
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
	// PrivateSandboxPorts requires an API key on the generic port proxy — the
	// URLs get_host(port) hands out for services running inside a sandbox.
	// Default (false) matches E2B, where possession of the unguessable
	// <port>-<sandboxID>.<domain> hostname is the capability and the URL is
	// simply fetchable. That default exists because a browser cannot attach a
	// header: demanding one does not make the common case (open the dev server
	// running in your sandbox, point a webhook at it) inconvenient, it makes it
	// impossible. Set this when every consumer is a program that can carry the
	// key and you would rather not rely on the hostname staying secret.
	// It never affects envd RPCs or the code interpreter: those authenticate
	// regardless.
	PrivateSandboxPorts bool
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
		// Resolve the target sandbox from the Host first: two E2B behaviours
		// below have to be decided *before* authenticating, and both of them
		// hinge on the sandbox being named by the (unguessable) hostname rather
		// than by a caller-supplied header.
		sandbox, isCode := "", false
		hostPort := 0 // >0 when the Host matched <port>-<sandboxID>.<domain>
		if envdHost != nil {
			if m := envdHost.FindStringSubmatch(hostOnly(r.Host)); m != nil {
				sandbox, isCode = m[2], m[1] == strconv.Itoa(codePort)
				hostPort, _ = strconv.Atoi(m[1])
			}
		}

		// (1) Sandbox.is_running() probes GET /health on the envd host and reads
		// 502 as "not running", anything else as running. Every way of failing —
		// killed, not ours, unreachable, or a credential that died with the
		// sandbox — has to answer 502, or the SDK raises instead of returning
		// False. Auth failure included: the SDK sends the *per-sandbox* envd
		// token, which stops being valid the moment the sandbox is killed.
		// Answering 502 leaks nothing — only an authenticated owner of a live
		// sandbox ever sees 200.
		healthProbe := hostPort > 0 && r.URL.Path == "/health"

		// (2) The code-interpreter SDK is inconsistent about credentials:
		// run_code POSTs /execute *with* X-Access-Token, but every /contexts
		// call (create/list/restart/remove) sends only E2b-Sandbox-Id and the
		// port header — no token at all. E2B's own topology accepts that,
		// treating possession of the <port>-<sandboxID>.<domain> hostname as the
		// capability, so rejecting it makes contexts unusable against the
		// unmodified SDK. Scope the tolerance as tightly as the wire allows:
		// the code-interpreter port only, /contexts only, and only when the
		// sandbox came from the Host — never from the caller-supplied header.
		codeCtxPath := strings.HasPrefix(r.URL.Path, "/contexts")
		hostScopedCtx := sandbox != "" && isCode && codeCtxPath

		// (3) The same capability model, for the generic port proxy: a plain
		// fetch of get_host(port) carries no credential and — from a browser —
		// never can. Scoped to in-sandbox services only; envd RPCs and the code
		// interpreter are excluded here and stay authenticated below.
		publicPort := !cfg.PrivateSandboxPorts && sandbox != "" && hostPort > 0 &&
			!isCode && !isEnvdPath(r.URL.Path) && !codeCtxPath && r.URL.Path != "/execute"

		// Authenticate. The control plane uses X-API-KEY / Authorization; envd
		// uses the X-Access-Token header (the sandbox's envd access token).
		key := presentedKey(r.Header.Get("X-API-KEY"), r.Header.Get("Authorization"))
		if key == "" {
			key = r.Header.Get("X-Access-Token")
		}
		ident, ok := cfg.Auth(key)
		if !ok {
			if healthProbe {
				writeErr(w, http.StatusBadGateway, "sandbox is not running")
				return
			}
			// E2B's code-interpreter also talks to the local Jupyter kernel
			// unauthenticated under E2B_DEBUG=true — there /execute carries no
			// credential either. In path/debug mode (no vanity domain) tolerate
			// that too, so the debug port stays usable against an auth-enabled
			// server; the single-sandbox resolver still scopes execution.
			debugCodePath := cfg.Domain == "" && key == "" &&
				(r.URL.Path == "/execute" || codeCtxPath)
			anonAllowed := key == "" && (hostScopedCtx || publicPort)
			if !debugCodePath && !anonAllowed {
				writeErr(w, http.StatusUnauthorized, "unauthorized: missing or invalid API key")
				return
			}
			ident = "" // anonymous; scoped by the resolver or by the Host
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxIdent, ident))

		// Fall back to the headers / single-sandbox resolver when the Host did
		// not name a sandbox (e.g. the fixed E2B_SANDBOX_URL in debug mode).
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

		// The is_running() probe itself (flag computed above).
		// /health is not an envd RPC path, so without this branch it falls
		// through to the generic port proxy, which dials 127.0.0.1:49983 inside
		// the container, finds nothing listening and returns 502 — reporting a
		// perfectly healthy sandbox as not running. api.<domain>/health is
		// unaffected: it is not an envd host, so it stays the control plane's.
		if healthProbe {
			if sandbox == "" && cfg.SandboxResolver != nil {
				sandbox = cfg.SandboxResolver(ident)
			}
			// Gone, not ours, or not answering all collapse to the same 502 the
			// SDK expects — and unlike the 404 used elsewhere it reveals nothing
			// extra, since a 200 already distinguishes a reachable sandbox.
			unreachable := sandbox == "" ||
				(cfg.Authorize != nil && !cfg.Authorize(ident, sandbox))
			if !unreachable && cfg.Forwarder != nil && cfg.Forwarder(w, r, sandbox) {
				return // tunnel lives on a peer node; it answered
			}
			if !unreachable && cfg.Envd != nil {
				_, err := cfg.Envd.Stat(sandbox, "/")
				unreachable = err != nil
			}
			if unreachable {
				writeErr(w, http.StatusBadGateway, "sandbox is not running")
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		isEnvd := isEnvdPath(r.URL.Path)
		isCodePath := r.URL.Path == "/execute" || strings.HasPrefix(r.URL.Path, "/contexts")
		if isEnvd || isCodePath {
			if sandbox == "" && cfg.SandboxResolver != nil {
				sandbox = cfg.SandboxResolver(ident)
			}
			// Ownership: the target sandbox came from a caller-controlled
			// header/Host, so verify the authenticated identity may reach it
			// before touching any tunnel. 404 (not 403) to avoid confirming a
			// sandbox the caller doesn't own even exists. Checked on the entry
			// node — Owner lives in the shared store, no tunnel needed — so an
			// unauthorized cross-tenant request never gets forwarded.
			if sandbox != "" && cfg.Authorize != nil && !cfg.Authorize(ident, sandbox) {
				writeErr(w, http.StatusNotFound, "sandbox not found")
				return
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
				// Ownership gate (see the envd/code branch above): the generic
				// port proxy reaches arbitrary in-sandbox services, so the same
				// cross-tenant check applies before forwarding or proxying.
				if cfg.Authorize != nil && !cfg.Authorize(ident, sandbox) {
					writeErr(w, http.StatusNotFound, "sandbox not found")
					return
				}
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
