// Copyright 2026 SandrPod Contributors

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestE2BHostRouterScope pins which hostnames the compatibility layer takes.
// It gets exactly the two shapes the E2B SDK addresses; every other name under
// the same domain stays with the native API, which is the primary surface —
// sandrpod-cli, the REST API, and the SDKs all talk to it, and turning on
// compatibility for existing E2B users must not leave it without a hostname.
func TestE2BHostRouterScope(t *testing.T) {
	const domain = "example.com"
	mark := func(who string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Served-By", who)
		})
	}
	h := e2bHostRouter(domain, mark("e2b"), mark("native"))
	servedPath := func(host, path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Header().Get("X-Served-By")
	}
	served := func(host string) string { return servedPath(host, "/sandboxes") }

	e2b := []string{
		"api." + domain,          // control plane
		"api." + domain + ":443", // port suffix must not confuse it
		"49983-sb1." + domain,    // envd
		"49999-sb1." + domain,    // code interpreter
		"8000-sb1." + domain,     // a user's service, via the port proxy
	}
	native := []string{
		"console." + domain, // where an operator would put the native API
		"admin." + domain,
		"www." + domain,
		domain,                   // the apex
		"api.other.com",          // a different domain entirely
		"notaport-sb1." + domain, // shaped like a sandbox host but is not one
		"8000-sb1.evil.com",      // right shape, wrong domain
	}
	for _, host := range e2b {
		if got := served(host); got != "e2b" {
			t.Errorf("%s: want e2b gateway, got %q", host, got)
		}
	}
	for _, host := range native {
		if got := served(host); got != "native" {
			t.Errorf("%s: want native API, got %q", host, got)
		}
	}

	// api.<domain> is shared: the split is by path, so nobody has to move to a
	// second hostname just because compatibility is on.
	api := "api." + domain
	for _, p := range []string{
		"/sandboxes", "/sandboxes/sb1", "/sandboxes/sb1/pause", "/sandboxes/metrics",
		"/v2/sandboxes", "/v3/templates", "/templates/t1", "/snapshots", "/volumes",
	} {
		if got := servedPath(api, p); got != "e2b" {
			t.Errorf("api%s: want e2b gateway, got %q", p, got)
		}
	}
	for _, p := range []string{
		"/api/v1/sandboxes", "/api/v1/poders", "/api/v1/tokens",
		"/health", // the SDK only probes health on the envd host, never here
		"/metrics", "/console", "/ws/poder/connect", "/",
	} {
		if got := servedPath(api, p); got != "native" {
			t.Errorf("api%s: want native API, got %q", p, got)
		}
	}

	// A sandbox host is the gateway's whole and entire — path plays no part.
	if got := servedPath("49983-sb1."+domain, "/api/v1/poders"); got != "e2b" {
		t.Errorf("sandbox host, native-looking path: want e2b gateway, got %q", got)
	}
}
