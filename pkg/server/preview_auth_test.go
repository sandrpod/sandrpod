// Copyright 2026 SandrPod Contributors
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPreviewTokenRoundTrip(t *testing.T) {
	secret := []byte("s3cret")
	tok := mintPreviewToken(secret, "box", 6901, time.Hour)
	if !verifyPreviewToken(secret, tok, "box", 6901) {
		t.Fatal("a freshly minted token must verify")
	}
	// Bound to sandbox and port: a token for one preview must not open another.
	if verifyPreviewToken(secret, tok, "other", 6901) {
		t.Fatal("token verified for a different sandbox")
	}
	if verifyPreviewToken(secret, tok, "box", 9222) {
		t.Fatal("token verified for a different port")
	}
	// Tampering and expiry.
	if verifyPreviewToken([]byte("other-secret"), tok, "box", 6901) {
		t.Fatal("token verified under a different secret")
	}
	expired := mintPreviewToken(secret, "box", 6901, -time.Minute)
	if verifyPreviewToken(secret, expired, "box", 6901) {
		t.Fatal("expired token verified")
	}
}

func TestPortProxyRequiresCredential(t *testing.T) {
	d := e2bDeps{cfg: Config{Token: "platform-token"}, sandboxes: nil}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if d.previewAuthorized(rec, r, "box", 6901) {
		t.Fatal("an unauthenticated preview request was allowed")
	}

	// A valid signed token passes and is persisted as a cookie for the page's
	// subresources and websocket.
	secret := d.cfg.previewSecret()
	tok := mintPreviewToken(secret, "box", 6901, time.Hour)
	r2 := httptest.NewRequest(http.MethodGet, "/?token="+tok, nil)
	rec2 := httptest.NewRecorder()
	if !d.previewAuthorized(rec2, r2, "box", 6901) {
		t.Fatal("a signed token was refused")
	}
	if !strings.Contains(rec2.Header().Get("Set-Cookie"), PreviewCookie) {
		t.Fatalf("expected the preview cookie to be set, got %q", rec2.Header().Get("Set-Cookie"))
	}

	// A platform API key is equally acceptable: the E2B SDK presents one and
	// must keep working.
	r3 := httptest.NewRequest(http.MethodGet, "/", nil)
	r3.Header.Set("Authorization", "Bearer platform-token")
	if !d.previewAuthorized(httptest.NewRecorder(), r3, "box", 6901) {
		t.Fatal("a valid platform token was refused")
	}
}

func TestPreviewURLEndpointShape(t *testing.T) {
	cfg := Config{Token: "t", E2BDomain: "sandbox.example.com"}
	u, err := cfg.previewTokenEndpoint("box", 6901)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://6901-box.sandbox.example.com/?token=") {
		t.Fatalf("unexpected preview URL %q", u)
	}
}
