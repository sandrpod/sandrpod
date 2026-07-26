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
	served := func(host string) string {
		req := httptest.NewRequest("GET", "/whatever", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Header().Get("X-Served-By")
	}

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
}
