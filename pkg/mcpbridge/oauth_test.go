package mcpbridge

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

// quietLogger keeps OAuth test output clean.
func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// ─── file token store ─────────────────────────────────────────────────────────

func TestFileTokenStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newFileTokenStore(dir, "notion")
	ctx := context.Background()

	// Empty store → ErrNoToken (the signal the OAuth transport keys off).
	if _, err := s.GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Fatalf("empty store: want ErrNoToken, got %v", err)
	}

	tok := &transport.Token{
		AccessToken:  "at-123",
		TokenType:    "Bearer",
		RefreshToken: "rt-456",
		ExpiresAt:    time.Now().Add(time.Hour).UTC(),
	}
	if err := s.SaveToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "at-123" || got.RefreshToken != "rt-456" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Token files hold refresh tokens — must be 0600 (owner-only).
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "notion.json"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("token file perm = %o, want 0600", perm)
		}
	}
}

func TestSanitizeFileName(t *testing.T) {
	if got := sanitizeFileName("github/owner repo:x"); got != "github_owner_repo_x" {
		t.Errorf("sanitizeFileName = %q", got)
	}
}

// ─── config opt-in ────────────────────────────────────────────────────────────

func TestServerConfig_WantsOAuth(t *testing.T) {
	if (ServerConfig{URL: "https://x/mcp"}).WantsOAuth() {
		t.Error("plain url entry must not want oauth")
	}
	if !(ServerConfig{URL: "https://x/mcp", Auth: "oauth"}).WantsOAuth() {
		t.Error(`auth:"oauth" should want oauth`)
	}
	if !(ServerConfig{URL: "https://x/mcp", OAuth: &OAuthServerOpts{Scopes: []string{"read"}}}).WantsOAuth() {
		t.Error("oauth{} block should imply oauth")
	}
	// oauth needs an HTTP entry and the streamable transport.
	if err := (ServerConfig{Command: "npx", Auth: "oauth"}).Validate(); err == nil {
		t.Error("auth=oauth on a stdio entry must fail validation")
	}
	if err := (ServerConfig{URL: "https://x/mcp", Type: "sse", Auth: "oauth"}).Validate(); err == nil {
		t.Error("auth=oauth with sse must fail validation")
	}
	if err := (ServerConfig{URL: "https://x/mcp", Auth: "oauth"}).Validate(); err != nil {
		t.Errorf("valid oauth entry rejected: %v", err)
	}
}

// ─── broker begin + callback ──────────────────────────────────────────────────

// stubFlow fakes *transport.OAuthHandler for broker tests.
type stubFlow struct {
	clientID   string
	registered bool
	processed  chan [3]string // code, state, verifier
	processErr error
}

func (f *stubFlow) GetClientID() string { return f.clientID }
func (f *stubFlow) RegisterClient(_ context.Context, _ string) error {
	f.registered = true
	f.clientID = "dcr-client"
	return nil
}
func (f *stubFlow) GetAuthorizationURL(_ context.Context, state, challenge string) (string, error) {
	return "https://auth.example.com/authorize?state=" + state + "&code_challenge=" + challenge, nil
}
func (f *stubFlow) ProcessAuthorizationResponse(_ context.Context, code, state, verifier string) error {
	if f.processed != nil {
		f.processed <- [3]string{code, state, verifier}
	}
	return f.processErr
}

func testBroker(t *testing.T) *oauthBroker {
	t.Helper()
	b := newOAuthBroker(OAuthOptions{
		TokenDir:     t.TempDir(),
		CallbackAddr: "127.0.0.1:0", // ephemeral; redirectURI uses the bound port
	}, quietLogger())
	if err := b.listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.close)
	return b
}

func TestOAuthBroker_BeginAndCallback(t *testing.T) {
	b := testBroker(t)
	restarted := make(chan string, 1)
	b.restartChild = func(name string) { restarted <- name }

	flow := &stubFlow{processed: make(chan [3]string, 1)}
	authURL, err := b.begin(context.Background(), "notion", flow)
	if err != nil {
		t.Fatal(err)
	}
	if !flow.registered {
		t.Error("expected dynamic client registration for empty client_id")
	}
	if authURL == "" {
		t.Fatal("empty auth URL")
	}

	// Extract the state the broker parked the flow under.
	b.mu.Lock()
	var state string
	for k := range b.pending {
		state = k
	}
	b.mu.Unlock()
	if state == "" {
		t.Fatal("no pending auth registered")
	}

	// Simulate the browser redirect hitting the loopback callback.
	resp, err := http.Get(b.redirectURI() + "?code=authcode-1&state=" + state)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", resp.StatusCode)
	}
	got := <-flow.processed
	if got[0] != "authcode-1" || got[1] != state || got[2] == "" {
		t.Fatalf("ProcessAuthorizationResponse got %v", got)
	}
	select {
	case name := <-restarted:
		if name != "notion" {
			t.Fatalf("restarted %q, want notion", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child restart not triggered after successful callback")
	}
	// The pending entry is consumed — a replayed callback must 4xx.
	resp2, err := http.Get(b.redirectURI() + "?code=authcode-1&state=" + state)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("replayed callback status = %d, want 400", resp2.StatusCode)
	}
}

func TestOAuthBroker_CallbackErrors(t *testing.T) {
	b := testBroker(t)
	failed := make(chan string, 1)
	b.failChild = func(name, reason string) { failed <- name + ": " + reason }

	// Unknown state → 400.
	resp, err := http.Get(b.redirectURI() + "?code=x&state=nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown state: status %d, want 400", resp.StatusCode)
	}

	// Provider returned error=access_denied → child marked failed.
	flow := &stubFlow{}
	if _, err := b.begin(context.Background(), "gh", flow); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	var state string
	for k := range b.pending {
		state = k
	}
	b.mu.Unlock()
	resp2, err := http.Get(b.redirectURI() + "?state=" + state + "&error=access_denied")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	select {
	case msg := <-failed:
		if msg == "" {
			t.Fatal("empty failure reason")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failChild not invoked on error callback")
	}
}

func TestOAuthBroker_ReBeginInvalidatesOldPending(t *testing.T) {
	b := testBroker(t)
	flow := &stubFlow{}
	if _, err := b.begin(context.Background(), "notion", flow); err != nil {
		t.Fatal(err)
	}
	if _, err := b.begin(context.Background(), "notion", flow); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	n := len(b.pending)
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("re-begin should keep exactly one pending per server, got %d", n)
	}
}

// ─── child parks in waiting_auth ──────────────────────────────────────────────

// stubRuntime fakes the manager-side oauthRuntime for child tests.
type stubRuntime struct {
	transport childTransport
	authURL   string
	notified  chan string
}

func (r *stubRuntime) transportFor(string, ServerConfig) (childTransport, error) {
	return r.transport, nil
}
func (r *stubRuntime) begin(context.Context, string, oauthFlow) (string, error) {
	return r.authURL, nil
}
func (r *stubRuntime) notify(_, url string) {
	if r.notified != nil {
		r.notified <- url
	}
}

// authRequiredTransport fails Start with mcp-go's authorization-required error.
type authRequiredTransport struct{ fakeTransport }

func (a *authRequiredTransport) Start(context.Context) error {
	return &transport.OAuthAuthorizationRequiredError{Handler: &transport.OAuthHandler{}}
}

func TestChild_ParksInWaitingAuth(t *testing.T) {
	rt := &stubRuntime{
		transport: &authRequiredTransport{},
		authURL:   "https://auth.example.com/authorize?state=s1",
		notified:  make(chan string, 1),
	}
	c := newChild("notion", ServerConfig{URL: "https://mcp.notion.com/mcp", Auth: "oauth"})
	c.oauth = rt

	err := c.Start(context.Background())
	if err == nil {
		t.Fatal("Start should propagate the authorization-required error")
	}
	if got := c.State(); got != StateWaitingAuth {
		t.Fatalf("state = %s, want waiting_auth", got)
	}
	if got := c.AuthURL(); got != rt.authURL {
		t.Fatalf("AuthURL = %q", got)
	}
	select {
	case url := <-rt.notified:
		if url != rt.authURL {
			t.Fatalf("notified with %q", url)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAuthorizationRequired hook not fired")
	}
}

func TestChild_OAuthEntryWithoutRuntimeFailsClearly(t *testing.T) {
	c := newChild("notion", ServerConfig{URL: "https://mcp.notion.com/mcp", Auth: "oauth"})
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("expected failure when bridge OAuth is disabled")
	}
	if got := c.State(); got != StateFailed {
		t.Fatalf("state = %s, want failed", got)
	}
}

// ─── manifest redaction ───────────────────────────────────────────────────────

func TestManifest_AuthURLRedactedOnPublicSurface(t *testing.T) {
	mgr := NewManager(ManagerOptions{ConfigPath: filepath.Join(t.TempDir(), "mcp.json"), Logger: quietLogger()})
	c := newChild("notion", ServerConfig{URL: "https://mcp.notion.com/mcp", Auth: "oauth"})
	c.setWaitingAuth("https://auth.example.com/authorize?secret-state")
	mgr.children["notion"] = c

	get := func(h http.Handler, path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	pub := get(NewHTTPHandler(mgr), "/mcp/manifest")
	if containsStr(pub, "secret-state") {
		t.Error("public manifest must redact auth_url")
	}
	if !containsStr(pub, "waiting_auth") {
		t.Error("public manifest should still show the waiting_auth state")
	}
	adm := get(NewAdminHandler(mgr), "/admin/manifest")
	if !containsStr(adm, "secret-state") {
		t.Error("admin manifest should include auth_url")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
