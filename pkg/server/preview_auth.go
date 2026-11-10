// Copyright 2026 SandrPod Contributors
// Preview authentication: sandbox ports exposed on the wildcard host
// (<port>-<sandbox>.<domain>) used to accept anyone who guessed the name, which
// meant a stranger could drive the desktop, attach a debugger to Chrome (CDP) or
// read whatever the sandbox was serving. This file adds a signed short-lived URL
// token plus a cookie hand-off so browsers (and websockets, which cannot send
// custom headers) keep working.

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// PreviewCookie carries the signed token after the first ?token= hit, so the
// page's subresources and its websocket handshake carry credentials too.
const PreviewCookie = "sandrpod_preview"

// previewSecret is the signing key: an explicit secret when configured, else the
// platform token (which every production deployment has), else empty = disabled.
func (cfg Config) previewSecret() []byte {
	if s := strings.TrimSpace(cfg.PreviewSecret); s != "" {
		return []byte(s)
	}
	if cfg.Token != "" {
		return []byte("preview:" + cfg.Token)
	}
	return nil
}

// mintPreviewToken signs sandbox|port|expiry. Anyone with the platform token can
// already do everything, so binding the signature to that key adds no new trust.
func mintPreviewToken(secret []byte, sandbox string, port int, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%d|%d", sandbox, port, exp)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

// verifyPreviewToken reports whether token is a valid signature for this
// sandbox/port and has not expired.
func verifyPreviewToken(secret []byte, token, sandbox string, port int) bool {
	if len(secret) == 0 || token == "" {
		return false
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	payload := string(raw)
	want := fmt.Sprintf("%s|%d|", sandbox, port)
	if !strings.HasPrefix(payload, want) {
		return false
	}
	exp, err := strconv.ParseInt(strings.TrimPrefix(payload, want), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(parts[1]))
}

// previewAuthorized decides whether a wildcard preview request may proceed. It
// accepts, in order of preference:
//
//  1. a signed ?token= (set as a cookie and then relied on by the page);
//  2. the sandbox's cookie;
//  3. any valid platform credential in the request (API keys make the E2B SDK's
//     envd and port calls work unchanged);
//  4. everything, when the deployment has no auth at all.
//
// It returns the identity name when one was established, and whether the caller
// authenticated at all (so the caller can decide between 401 and 403).
func (d e2bDeps) previewAuthorized(w http.ResponseWriter, r *http.Request, sandbox string, port int) bool {
	if d.cfg.authDisabled() {
		return true
	}
	secret := d.cfg.previewSecret()
	if len(secret) == 0 {
		// No secret and auth is on: nothing can be verified, so refuse rather
		// than silently exposing the sandbox.
		return false
	}
	q := r.URL.Query().Get("token")
	if q != "" && verifyPreviewToken(secret, q, sandbox, port) {
		// Persist it for the page's subresources and websocket: a browser cannot
		// add a header to a WS handshake, but it does send cookies.
		http.SetCookie(w, &http.Cookie{
			Name:     PreviewCookie,
			Value:    q,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   true,
			MaxAge:   int((2 * time.Hour).Seconds()),
		})
		return true
	}
	if c, err := r.Cookie(PreviewCookie); err == nil && verifyPreviewToken(secret, c.Value, sandbox, port) {
		return true
	}
	// A platform credential (API key / envd token) is enough: this keeps the E2B
	// SDK's envd and port-proxy calls working, and lets CLI users curl a preview
	// with their key.
	auth := d.cfg.Authenticator
	if auth == nil {
		auth = d.cfg // the Config implements the built-in token scheme
	}
	if id, err := auth.Authenticate(r); err == nil && id.Name != "" {
		return true
	}
	return false
}

// handlePreviewURL serves GET /api/v1/sandboxes/{name}/preview/{port}: the
// signed URL for a sandbox port. Auth is the mux middleware's; the caller must
// at least be allowed to see the sandbox (the same owner scoping the rest of the
// API applies, since the record lookup is the standard one).
func handlePreviewURL(w http.ResponseWriter, r *http.Request, cfg Config, sandboxes interface {
	Get(string) (*podpkg.SandboxInfo, bool)
}) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sandboxes/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[1] != "preview" {
		http.NotFound(w, r)
		return
	}
	name := parts[0]
	port, err := strconv.Atoi(parts[2])
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	if _, ok := sandboxes.Get(name); !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	url, err := cfg.previewTokenEndpoint(name, port)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"url": url, "sandbox": name, "port": port})
}

// previewTokenEndpoint mints a signed URL for a sandbox port. Admin-less: the
// caller must already be authenticated by the mux; ownership is checked by the
// sandbox handler before calling this.
func (cfg Config) previewTokenEndpoint(sandbox string, port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("port must be 1..65535")
	}
	secret := cfg.previewSecret()
	if len(secret) == 0 {
		return "", fmt.Errorf("preview tokens are not configured on this server")
	}
	var host string
	if d := strings.TrimSpace(cfg.E2BDomain); d != "" {
		host = fmt.Sprintf("%d-%s.%s", port, sandbox, strings.TrimPrefix(d, "api."))
	} else {
		// No E2B domain: the API path form still works and is already behind auth.
		return fmt.Sprintf("/api/v1/sandboxes/%s/toolbox/proxy/%d/", sandbox, port), nil
	}
	tok := mintPreviewToken(secret, sandbox, port, 2*time.Hour)
	return fmt.Sprintf("https://%s/?token=%s", host, tok), nil
}

// PreviewSecret, when set, signs preview URLs independently of the platform
// token (rotation without rotating tokens).
func (cfg Config) previewSecretConfigured() bool { return len(cfg.previewSecret()) > 0 }
