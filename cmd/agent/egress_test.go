// Copyright 2026 SandrPod
// Wiring test for -egress-mode: the flag turns into a proxy, the proxy turns
// into environment variables, and a spawned process inherits them. A unit test
// of pkg/egress cannot see that path, and that path is where the feature is
// either wired up or not.

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		// A server-mode test leaves a sync goroutine behind; stop it before
		// the next test restores the flags it would otherwise be reading.
		if egressSyncStop != nil {
			egressSyncStop()
			egressSyncStop = nil
		}
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

// ─── control-plane policy (mode=server) ─────────────────────────────────────

func withControlPlane(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	oldURL, oldToken, oldName := *apiURL, *token, *name
	*apiURL, *token, *name = srv.URL, "test-token", "sb-egress"
	t.Cleanup(func() { *apiURL, *token, *name = oldURL, oldToken, oldName })
	return srv
}

func TestFetchEgressPolicy(t *testing.T) {
	withControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want the agent's token", got)
		}
		if r.URL.Path != "/api/v1/sandboxes/sb-egress/egress" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"sandbox":"sb-egress","policy":{"allowed_domains":["api.example.com"]}}`)
	})

	p, err := fetchEgressPolicy(context.Background(), egressTarget{apiURL: *apiURL, token: *token, sandbox: *name})
	if err != nil {
		t.Fatalf("fetchEgressPolicy: %v", err)
	}
	if p == nil || len(p.AllowedDomains) != 1 || p.AllowedDomains[0] != "api.example.com" {
		t.Fatalf("policy = %+v", p)
	}
}

// A sandbox the control plane knows nothing about must read as "deny all",
// not as an error that leaves the previous (possibly wider) policy in place.
func TestFetchEgressPolicy_NoPolicyMeansDenyAll(t *testing.T) {
	withControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"sandbox":"sb-egress","policy":null}`)
	})
	p, err := fetchEgressPolicy(context.Background(), egressTarget{apiURL: *apiURL, token: *token, sandbox: *name})
	if err != nil {
		t.Fatalf("fetchEgressPolicy: %v", err)
	}
	if p == nil || len(p.AllowedDomains) != 0 {
		t.Fatalf("null policy should become an empty allow-list, got %+v", p)
	}

}

// A 404 means "this sandbox is not registered yet", which the sync loop retries
// on a short timer. It must not be confused with "registered, no policy":
// collapsing the two costs a full sync interval of egress after every restart.
func TestFetchEgressPolicy_NotFoundIsNotRegistered(t *testing.T) {
	withControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sandbox not found", http.StatusNotFound)
	})
	if _, err := fetchEgressPolicy(context.Background(), egressTarget{apiURL: *apiURL, token: *token, sandbox: *name}); !errors.Is(err, errSandboxNotRegistered) {
		t.Fatalf("404 should report errSandboxNotRegistered, got %v", err)
	}
}

func TestFetchEgressPolicy_ServerErrorIsAnError(t *testing.T) {
	withControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := fetchEgressPolicy(context.Background(), egressTarget{apiURL: *apiURL, token: *token, sandbox: *name}); err == nil {
		t.Fatal("a 500 must be reported as an error so the current policy is kept")
	}
}

// Server mode starts closed and opens only when a policy actually arrives.
func TestInstallEgress_ServerModeStartsClosedThenSyncs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, upPort, _ := net.SplitHostPort(upURL.Host)

	withControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"sandbox":"sb-egress","policy":{"allowed_domains":["127.0.0.1:`+upPort+`"]}}`)
	})
	withEgressFlags(t, "server", "")
	*egressSync = 5 * time.Second
	t.Cleanup(func() { *egressSync = 60 * time.Second })

	if err := installEgress(); err != nil {
		t.Fatalf("installEgress: %v", err)
	}
	if startedEgress == nil {
		t.Fatal("server mode should have started a proxy")
	}

	proxyURL, err := url.Parse(startedEgress.ProxyURL())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}

	// The first sync runs immediately in a goroutine; wait for it to land.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := client.Get(upstream.URL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("policy from the control plane never took effect")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The retry path in miniature: an agent that reaches the control plane before
// its sandbox exists must not sit on deny-all until the next full interval.
func TestSyncEgressPolicy_RetriesUntilRegistered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, upPort, _ := net.SplitHostPort(upURL.Host)

	var registered atomic.Bool
	withControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if !registered.Load() {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"sandbox":"sb-egress","policy":{"allowed_domains":["127.0.0.1:`+upPort+`"]}}`)
	})
	// A long interval: only the registration retry can make this pass in time.
	withEgressFlags(t, "server", "")
	*egressSync = time.Hour
	t.Cleanup(func() { *egressSync = 60 * time.Second })

	if err := installEgress(); err != nil {
		t.Fatalf("installEgress: %v", err)
	}

	// While unregistered the sandbox has no egress.
	proxyURL, _ := url.Parse(startedEgress.ProxyURL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request while unregistered: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("while unregistered: status=%d, want 403", resp.StatusCode)
	}

	// The sandbox appears; the fast retry (2s) must pick the policy up well
	// before the one-hour interval would.
	registered.Store(true)
	deadline := time.Now().Add(6 * time.Second)
	for {
		resp, err := client.Get(upstream.URL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("policy was not fetched after the sandbox registered")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
