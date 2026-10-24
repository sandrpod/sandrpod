// Copyright 2026 SandrPod
// Wiring test for -egress-mode: the flag turns into a proxy, the proxy turns
// into environment variables, and a spawned process inherits them. A unit test
// of pkg/egress cannot see that path, and that path is where the feature is
// either wired up or not.

package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/toolbox"
)

// withEgressFlags sets the package-level flag values for one test and restores
// them (and the child environment) afterwards.
func withEgressFlags(t *testing.T, mode, policy string) {
	t.Helper()
	oldMode, oldPolicy := *egressMode, *egressPolicy
	*egressMode, *egressPolicy = mode, policy
	t.Cleanup(func() {
		*egressMode, *egressPolicy = oldMode, oldPolicy
		toolbox.SetChildEnv(nil)
	})
}

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "egress.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

func TestInstallEgress_OffIsANoOp(t *testing.T) {
	withEgressFlags(t, "off", "")
	if err := installEgress(); err != nil {
		t.Fatalf("off mode should not fail: %v", err)
	}
	for _, kv := range toolbox.ChildEnv() {
		if strings.HasPrefix(kv, "HTTP_PROXY=") && strings.Contains(kv, "sandbox:") {
			t.Fatalf("off mode installed a proxy: %q", kv)
		}
	}
}

func TestInstallEgress_EnforceNeedsPolicy(t *testing.T) {
	withEgressFlags(t, "enforce", "")
	err := installEgress()
	if err == nil {
		t.Fatal("enforce without a policy must refuse to start")
	}
	if !strings.Contains(err.Error(), "egress-policy") {
		t.Errorf("error should name the missing flag: %v", err)
	}
}

func TestInstallEgress_UnknownMode(t *testing.T) {
	withEgressFlags(t, "strict", writePolicy(t, `{"allowed_domains":[]}`))
	if err := installEgress(); err == nil {
		t.Fatal("an unknown mode must be rejected")
	}
}

func TestInstallEgress_BadPolicyIsAStartupError(t *testing.T) {
	// A typo'd key would otherwise produce an empty allow-list that silently
	// denies everything, which is indistinguishable from a broken network.
	withEgressFlags(t, "enforce", writePolicy(t, `{"allowed_domain":["example.com"]}`))
	if err := installEgress(); err == nil {
		t.Fatal("a policy with an unknown field must be rejected at startup")
	}
}

// The end-to-end shape: flag → proxy → child env → a real refusal.
func TestInstallEgress_InjectsProxyAndEnforces(t *testing.T) {
	withEgressFlags(t, "enforce", writePolicy(t, `{"allowed_domains":["api.example.com"]}`))
	if err := installEgress(); err != nil {
		t.Fatalf("installEgress: %v", err)
	}

	// The sandbox environment carries the proxy, with its token.
	var proxyURL string
	for _, kv := range toolbox.ChildEnv() {
		if strings.HasPrefix(kv, "HTTP_PROXY=") {
			proxyURL = strings.TrimPrefix(kv, "HTTP_PROXY=")
		}
	}
	if proxyURL == "" {
		t.Fatal("HTTP_PROXY was not installed into the child environment")
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parse installed proxy URL: %v", err)
	}
	if u.User == nil || u.User.String() == "sandbox:" {
		t.Fatalf("the installed proxy URL should carry a token, got %q", proxyURL)
	}
	if host := u.Hostname(); host != "127.0.0.1" {
		t.Fatalf("the proxy must bind loopback, got %q", host)
	}

	// A process using that environment is refused a destination the policy
	// does not name.
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(u)},
	}
	resp, err := client.Get("http://example.org/")
	if err != nil {
		t.Fatalf("request through the injected proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Deny-Reason"); got != "not_allowed" {
		t.Errorf("X-Deny-Reason = %q, want not_allowed", got)
	}
	if !strings.Contains(string(body), "not on the egress allow list") {
		t.Errorf("body should explain the refusal: %q", body)
	}
}
