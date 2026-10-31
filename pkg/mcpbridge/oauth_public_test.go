package mcpbridge

import "testing"

// A container sandbox has no browser on its loopback: the OAuth redirect must
// use the preview URL the E2B gateway can route. The control plane sees a
// one-time code; the token exchange happens inside the sandbox.
func TestRedirectURI_PublicCallbackBase(t *testing.T) {
	b := &oauthBroker{opts: OAuthOptions{
		CallbackAddr:       "127.0.0.1:7099",
		PublicCallbackBase: "https://7099-test.sandbox.sandrpod.com",
	}}
	got := b.redirectURI()
	want := "https://7099-test.sandbox.sandrpod.com/callback"
	if got != want {
		t.Fatalf("redirectURI = %q, want %q", got, want)
	}
}

// An agent host (no public base configured) keeps the loopback redirect.
func TestRedirectURI_LoopbackDefault(t *testing.T) {
	b := &oauthBroker{opts: OAuthOptions{CallbackAddr: "127.0.0.1:7099"}}
	if got, want := b.redirectURI(), "http://127.0.0.1:7099/callback"; got != want {
		t.Fatalf("redirectURI = %q, want %q", got, want)
	}
}

// A trailing slash on the base must not double up the path.
func TestRedirectURI_TrailingSlashTrimmed(t *testing.T) {
	b := &oauthBroker{opts: OAuthOptions{
		PublicCallbackBase: "https://7099-sb.sandbox.sandrpod.com/",
	}}
	if got, want := b.redirectURI(), "https://7099-sb.sandbox.sandrpod.com/callback"; got != want {
		t.Fatalf("redirectURI = %q, want %q", got, want)
	}
}
