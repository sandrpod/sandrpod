// Copyright 2026 SandrPod
// Wiring test for -egress-mode: the flag turns into a proxy, the proxy turns
// into environment variables, and a spawned process inherits them. A unit test
// of pkg/egress cannot see that path, and that path is where the feature is
// either wired up or not.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/permission"
	"github.com/sandrpod/sandrpod/pkg/sandbox"
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

// ─── OS sandbox wiring ──────────────────────────────────────────────────────

func withOSSandboxFlag(t *testing.T, mode string) {
	t.Helper()
	old := *osSandbox
	*osSandbox = mode
	t.Cleanup(func() {
		*osSandbox = old
		toolbox.SetSandbox(nil, sandbox.Policy{})
	})
}

func TestInstallOSSandbox_OffIsANoOp(t *testing.T) {
	withOSSandboxFlag(t, "off")
	if err := installOSSandbox(toolbox.NewServer("", "")); err != nil {
		t.Fatalf("off: %v", err)
	}
	if got := toolbox.SandboxName(); got != "" {
		t.Errorf("off installed a wrapper: %q", got)
	}
}

func TestInstallOSSandbox_UnknownMode(t *testing.T) {
	withOSSandboxFlag(t, "sandboxy")
	if err := installOSSandbox(toolbox.NewServer("", "")); err == nil {
		t.Fatal("an unknown mode must be rejected")
	}
}

// Forcing a backend that cannot run here must fail startup rather than start an
// agent that believes it is sandboxed. The probe is what decides, and on a
// machine where Seatbelt cannot be applied (an already-sandboxed shell, an MDM
// agent) it fails — which is exactly the case this asserts.
func TestInstallOSSandbox_ForcedBackendThatCannotRun(t *testing.T) {
	mode := "seatbelt"
	if runtime.GOOS == "linux" {
		mode = "bwrap"
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no OS sandbox backend on this platform")
	}
	if _, err := sandbox.Resolve(sandbox.Mode(mode), "", ""); err == nil {
		t.Skipf("%s is usable here, so the failure path cannot be exercised", mode)
	}

	withOSSandboxFlag(t, mode)
	if err := installOSSandbox(toolbox.NewServer("", "")); err == nil {
		t.Fatalf("forcing %s where it cannot run must fail startup", mode)
	}
}

// auto never fails startup: it either installs a backend or says why not.
func TestInstallOSSandbox_AutoNeverFailsStartup(t *testing.T) {
	withOSSandboxFlag(t, "auto")
	if err := installOSSandbox(toolbox.NewServer("", "")); err != nil {
		t.Fatalf("auto should not fail startup: %v", err)
	}
}

// The OS sandbox profile must include the persistence paths the gate refuses:
// the same list, enforced by the kernel.
func TestInstallOSSandbox_CarriesMandatoryDenies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only the Linux backend can be probed from a test")
	}
	mode := "bwrap"
	if _, err := sandbox.Resolve(sandbox.Mode(mode), "", ""); err != nil {
		t.Skipf("bwrap unusable here: %v", err)
	}

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".bashrc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldDepth := *mandatoryDenyDepth
	*mandatoryDenyDepth = 3
	t.Cleanup(func() { *mandatoryDenyDepth = oldDepth })

	withOSSandboxFlag(t, mode)

	tb := toolbox.NewServer("", "")
	// Point the toolbox at the tree we built. The working directory is process
	// state, so it is restored before the temp dir disappears: leaving it on a
	// deleted path breaks every later test that shells out (go build, update
	// commands) with "cannot determine current directory".
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := installOSSandbox(tb); err != nil {
		t.Fatalf("installOSSandbox: %v", err)
	}
	if got := toolbox.SandboxName(); got != "bwrap" {
		t.Fatalf("expected bwrap to be installed, got %q", got)
	}
	if !permission.IsMandatoryDenied(filepath.Join(work, ".git", "hooks", "pre-commit")) {
		t.Error("the gate's own checker disagrees with the profile's intent")
	}
}

// The profile the agent builds carries both halves of the policy: what must not
// be read (hardlocks) and what must not be written (persistence paths).
func TestSandboxPolicyFor(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "src", "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := sandboxPolicyFor(work, 3)
	if p.WorkDir != work {
		t.Errorf("work_dir = %q", p.WorkDir)
	}
	var sawHooks bool
	for _, d := range p.WriteDeny {
		if d == filepath.Join(work, ".git", "hooks") {
			sawHooks = true
		}
		if strings.Contains(d, "src") {
			t.Errorf("an ordinary source path must not be write-denied: %q", d)
		}
	}
	if !sawHooks {
		t.Errorf("git hooks missing from the write-deny list: %v", p.WriteDeny)
	}
	if p.NetworkNone {
		t.Error("network isolation must not be on without an egress relay")
	}
}

// ─── decider mode (per-connection decisions) ────────────────────────────────

func TestFetchEgressDecision(t *testing.T) {
	withControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/policy/decide" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body struct {
			Sandbox string `json:"sandbox"`
			Host    string `json:"host"`
			Port    int    `json:"port"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Sandbox != "sb-egress" || body.Host != "example.com" || body.Port != 443 {
			t.Errorf("unexpected decide body: %+v", body)
		}
		_, _ = io.WriteString(w, `{"allow":false,"code":"not_allowed","reason":"nope"}`)
	})

	v, err := fetchEgressDecision(context.Background(), egressTarget{apiURL: *apiURL, token: *token, sandbox: *name}, "example.com", 443)
	if err != nil {
		t.Fatalf("fetchEgressDecision: %v", err)
	}
	if v.Allow || v.Reason != "nope" || v.Code != "not_allowed" {
		t.Fatalf("verdict = %+v", v)
	}
}

// An unreachable control plane must not become an open proxy: the proxy refuses
// what it cannot decide, with a code that says which of the two it was.
func TestInstallEgress_DeciderModeDeniesWhenThePlaneIsDown(t *testing.T) {
	// Point at a server that closes immediately: every decision fails.
	withControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	withEgressFlags(t, "decider", "")

	if err := installEgress(); err != nil {
		t.Fatalf("installEgress: %v", err)
	}
	if startedEgress == nil {
		t.Fatal("decider mode should have started a proxy")
	}

	proxyURL, err := url.Parse(startedEgress.ProxyURL())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	resp, err := client.Get("http://example.org/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Deny-Reason"); got != "decider_unavailable" {
		t.Errorf("X-Deny-Reason = %q, want decider_unavailable", got)
	}
}

// ─── credential masking wiring ──────────────────────────────────────────────

func TestCredentialMasker_ReadsTheAgentsEnvironment(t *testing.T) {
	withCredentialFlags(t, "OPENAI_API_KEY,GITHUB_TOKEN", "", false)
	t.Setenv("OPENAI_API_KEY", "sk-real-1")
	t.Setenv("GITHUB_TOKEN", "ghp-real-2")

	m := credentialMasker()
	if m == nil {
		t.Fatal("no masker was built")
	}
	if m.Len() != 2 {
		t.Fatalf("masked %d credentials, want 2", m.Len())
	}
	env := strings.Join(m.Env(), "\n")
	if strings.Contains(env, "sk-real-1") || strings.Contains(env, "ghp-real-2") {
		t.Fatalf("the injected environment carries a real value:\n%s", env)
	}
}

// Nothing to mask means nothing to terminate: masking an unset variable would
// silently disable TLS termination for the whole sandbox.
func TestCredentialMasker_EmptyWhenNothingIsSet(t *testing.T) {
	withCredentialFlags(t, "NOT_SET_ANYWHERE", "", false)
	if m := credentialMasker(); m != nil {
		t.Fatalf("expected no masker, got %d credentials", m.Len())
	}
}

func TestCredentialMasker_LinksAWS(t *testing.T) {
	withCredentialFlags(t, "AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY", "", false)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAREAL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SECRETREAL")

	m := credentialMasker()
	if m == nil {
		t.Fatal("no masker")
	}
	// A SigV4 request signed with the sentinel key is recognized as linked.
	req, _ := http.NewRequest(http.MethodGet, "https://s3.amazonaws.com/", nil)
	req.Header.Set("X-Amz-Date", "20250101T000000Z")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+m.Sentinels()["AWS_ACCESS_KEY_ID"]+
		"/20250101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")
	resigned, err := m.ResignAWS(req)
	if err != nil {
		t.Fatalf("resign: %v", err)
	}
	if !resigned {
		t.Fatal("the AWS pair was not linked, so SigV4 would not be re-signed")
	}
	if strings.Contains(req.Header.Get("Authorization"), "fake_value_") {
		t.Error("the sentinel survived")
	}
}

func TestMaskedFiles_RewritesTheConfiguredFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")
	if err := os.WriteFile(path, []byte("token = REALTOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withCredentialFlags(t, "TOKEN", path, false)
	t.Setenv("TOKEN", "REALTOKEN")

	m := credentialMasker()
	if m == nil {
		t.Fatal("no masker")
	}
	files := maskedFiles(m)
	if len(files) != 1 {
		t.Fatalf("masked %d files, want 1", len(files))
	}
	raw, err := os.ReadFile(files[0].FakePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "REALTOKEN") {
		t.Fatal("the masked copy still holds the real value")
	}
}

func withCredentialFlags(t *testing.T, names, files string, plaintext bool) {
	t.Helper()
	oldNames, oldFiles, oldPlain := *credentialMask, *credentialMaskFiles, *credentialPlaintext
	*credentialMask, *credentialMaskFiles, *credentialPlaintext = names, files, plaintext
	t.Cleanup(func() { *credentialMask, *credentialMaskFiles, *credentialPlaintext = oldNames, oldFiles, oldPlain })
}
