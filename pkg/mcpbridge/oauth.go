// Copyright 2024 SandrPod
// Native OAuth for remote MCP servers (Notion/GitHub/Linear-style endpoints
// that follow the MCP authorization spec: OAuth 2.1 + PKCE + dynamic client
// registration). A server entry opts in with `"auth": "oauth"`; the child then
// uses mcp-go's OAuth-aware Streamable-HTTP client. When no token is stored
// yet, startup surfaces an authorization-required error and the child parks in
// StateWaitingAuth with an authorization URL. A human approves it in a browser;
// the loopback callback server exchanges the code, persists the token (file
// TokenStore, survives restarts), and restarts the child. Refresh thereafter is
// handled inside mcp-go with the stored refresh_token — unattended.

package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
)

// OAuthOptions configures the bridge-wide OAuth machinery. Nil (the default)
// disables it: entries with `"auth": "oauth"` then fail with a clear error.
type OAuthOptions struct {
	// TokenDir is where per-server token JSON files live (0700 dir, 0600
	// files). Tokens include refresh_tokens — this is credential material and
	// must never leave the machine.
	TokenDir string
	// CallbackAddr is the loopback listen address for the OAuth redirect,
	// e.g. "127.0.0.1:7099". The browser must be able to reach it, which is
	// true on an agent (employee PC) and generally NOT true inside a toolbox
	// container — OAuth entries are an agent-first feature.
	CallbackAddr string
	// ClientName is the client_name presented during RFC7591 dynamic client
	// registration. Defaults to "sandrpod-mcp-bridge".
	ClientName string
	// OnAuthorizationRequired fires when a child parks in waiting_auth.
	// The agent wires this to open the system browser; leave nil to only
	// surface the URL via the admin manifest.
	OnAuthorizationRequired func(serverName, authURL string)
}

const (
	defaultCallbackAddr = "127.0.0.1:7099"
	defaultClientName   = "sandrpod-mcp-bridge"
	pendingAuthTTL      = 10 * time.Minute
	callbackPath        = "/callback"
)

// oauthFlow is the slice of *transport.OAuthHandler the broker drives.
// Interface so tests can stub the flow without a live authorization server.
type oauthFlow interface {
	GetClientID() string
	RegisterClient(ctx context.Context, clientName string) error
	GetAuthorizationURL(ctx context.Context, state, codeChallenge string) (string, error)
	ProcessAuthorizationResponse(ctx context.Context, code, state, codeVerifier string) error
}

// oauthRuntime is what a Child sees. Interface so child tests can fake it.
type oauthRuntime interface {
	// transportFor builds the OAuth-aware MCP client for an opted-in entry.
	transportFor(name string, cfg ServerConfig) (childTransport, error)
	// begin drives discovery/registration and returns the authorization URL,
	// parking a pending flow keyed by the OAuth state parameter.
	begin(ctx context.Context, serverName string, flow oauthFlow) (string, error)
	// notify fires the operator hook (e.g. open browser) for a parked child.
	notify(serverName, authURL string)
}

// ─── file token store ─────────────────────────────────────────────────────────

// fileTokenStore persists one OAuth token as JSON on disk. Implements
// transport.TokenStore. Survives bridge restarts so authorization is a
// once-per-server ceremony, and refresh keeps working unattended.
type fileTokenStore struct {
	path string
	mu   sync.Mutex
}

func newFileTokenStore(dir, serverName string) *fileTokenStore {
	return &fileTokenStore{path: filepath.Join(dir, sanitizeFileName(serverName)+".json")}
}

func (s *fileTokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, transport.ErrNoToken
	}
	if err != nil {
		return nil, fmt.Errorf("read token %s: %w", s.path, err)
	}
	var tok transport.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, fmt.Errorf("parse token %s: %w", s.path, err)
	}
	return &tok, nil
}

func (s *fileTokenStore) SaveToken(ctx context.Context, tok *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a crash never leaves a truncated token file.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	return os.Rename(tmp, s.path)
}

// sanitizeFileName maps a server key to a safe filename component.
func sanitizeFileName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, name)
}

// ─── broker ───────────────────────────────────────────────────────────────────

// oauthBroker owns the loopback callback server and the pending-authorization
// table. One per ChildManager.
type oauthBroker struct {
	opts   OAuthOptions
	logger *log.Logger

	// restartChild / failChild are wired to the manager so a completed (or
	// failed) authorization moves the child out of waiting_auth.
	restartChild func(serverName string)
	failChild    func(serverName, reason string)

	mu      sync.Mutex
	ln      net.Listener
	srv     *http.Server
	pending map[string]*pendingAuth // key: OAuth state parameter
}

type pendingAuth struct {
	server   string
	flow     oauthFlow
	verifier string
	created  time.Time
}

func newOAuthBroker(opts OAuthOptions, logger *log.Logger) *oauthBroker {
	if opts.CallbackAddr == "" {
		opts.CallbackAddr = defaultCallbackAddr
	}
	if opts.ClientName == "" {
		opts.ClientName = defaultClientName
	}
	return &oauthBroker{opts: opts, logger: logger, pending: map[string]*pendingAuth{}}
}

// listen binds the loopback callback listener. Called once at manager start so
// the redirect URI (which must be known before any client is built) is stable.
func (b *oauthBroker) listen() error {
	ln, err := net.Listen("tcp", b.opts.CallbackAddr)
	if err != nil {
		return fmt.Errorf("oauth callback listen %s: %w", b.opts.CallbackAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, b.handleCallback)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	b.mu.Lock()
	b.ln = ln
	b.srv = srv
	b.mu.Unlock()
	// Capture srv locally: close() nils b.srv under the lock, and the serve
	// goroutine must not race on that field.
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.logger.Printf("mcpbridge: oauth callback server: %v", err)
		}
	}()
	b.logger.Printf("mcpbridge: oauth callback listening on http://%s%s", ln.Addr(), callbackPath)
	return nil
}

func (b *oauthBroker) close() {
	b.mu.Lock()
	srv := b.srv
	b.srv, b.ln = nil, nil
	b.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// redirectURI returns the callback URL the browser will be sent back to.
// Uses the bound address so an ephemeral ":0" port works too.
func (b *oauthBroker) redirectURI() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	addr := b.opts.CallbackAddr
	if b.ln != nil {
		addr = b.ln.Addr().String()
	}
	return "http://" + addr + callbackPath
}

// transportFor builds the OAuth-aware Streamable-HTTP client for an entry.
func (b *oauthBroker) transportFor(name string, cfg ServerConfig) (childTransport, error) {
	typ := strings.ToLower(strings.TrimSpace(cfg.Type))
	if typ != "" && typ != "http" && !strings.HasPrefix(typ, "streamable") {
		return nil, fmt.Errorf("auth=oauth requires the streamable-http transport (got type=%q)", cfg.Type)
	}
	url := expandEnv(cfg.URL, cfg.Env)
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = expandEnv(v, cfg.Env)
	}
	oc := client.OAuthConfig{
		RedirectURI: b.redirectURI(),
		TokenStore:  newFileTokenStore(b.opts.TokenDir, name),
		PKCEEnabled: true,
	}
	if o := cfg.OAuth; o != nil {
		oc.ClientID = expandEnv(o.ClientID, cfg.Env)
		oc.ClientSecret = expandEnv(o.ClientSecret, cfg.Env)
		oc.Scopes = o.Scopes
	}
	var opts []transport.StreamableHTTPCOption
	if len(headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(headers))
	}
	cli, err := client.NewOAuthStreamableHttpClient(url, oc, opts...)
	if err != nil {
		return nil, fmt.Errorf("create oauth MCP client: %w", err)
	}
	return &realChildTransport{c: cli}, nil
}

// begin runs discovery (+DCR when no client_id) and returns the authorization
// URL for the human step, parking the flow until the callback lands.
func (b *oauthBroker) begin(ctx context.Context, serverName string, flow oauthFlow) (string, error) {
	verifier, err := transport.GenerateCodeVerifier()
	if err != nil {
		return "", fmt.Errorf("generate code verifier: %w", err)
	}
	challenge := transport.GenerateCodeChallenge(verifier)
	state, err := transport.GenerateState()
	if err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	// Dynamic client registration (RFC7591) when the entry didn't pin a
	// client_id — the norm for MCP OAuth servers.
	if flow.GetClientID() == "" {
		if err := flow.RegisterClient(ctx, b.opts.ClientName); err != nil {
			return "", fmt.Errorf("dynamic client registration: %w", err)
		}
	}
	authURL, err := flow.GetAuthorizationURL(ctx, state, challenge)
	if err != nil {
		return "", fmt.Errorf("build authorization url: %w", err)
	}

	b.mu.Lock()
	// Drop stale pendings (user never finished the browser step).
	for k, p := range b.pending {
		if time.Since(p.created) > pendingAuthTTL {
			delete(b.pending, k)
		}
	}
	// One pending per server: a re-begin (child restart) invalidates the
	// previous URL, matching the handler's expectedState which was rotated by
	// GetAuthorizationURL above.
	for k, p := range b.pending {
		if p.server == serverName {
			delete(b.pending, k)
		}
	}
	b.pending[state] = &pendingAuth{server: serverName, flow: flow, verifier: verifier, created: time.Now()}
	b.mu.Unlock()
	return authURL, nil
}

func (b *oauthBroker) notify(serverName, authURL string) {
	if b.opts.OnAuthorizationRequired != nil {
		b.opts.OnAuthorizationRequired(serverName, authURL)
	}
}

// handleCallback is the browser redirect target: exchanges the code, persists
// the token via the flow's TokenStore, and restarts the child.
func (b *oauthBroker) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")

	b.mu.Lock()
	p := b.pending[state]
	if p != nil {
		delete(b.pending, state)
	}
	b.mu.Unlock()

	if state == "" || p == nil {
		httpHTML(w, http.StatusBadRequest, "授权回调无效或已过期",
			"unknown or expired state — restart the server entry to get a fresh authorization link.")
		return
	}
	if errCode := q.Get("error"); errCode != "" {
		desc := q.Get("error_description")
		reason := strings.TrimSpace("authorization denied: " + errCode + " " + desc)
		b.logger.Printf("mcpbridge: oauth %q: %s", p.server, reason)
		if b.failChild != nil {
			b.failChild(p.server, reason)
		}
		httpHTML(w, http.StatusOK, "授权被拒绝", reason)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.flow.ProcessAuthorizationResponse(ctx, q.Get("code"), state, p.verifier); err != nil {
		b.logger.Printf("mcpbridge: oauth %q: token exchange failed: %v", p.server, err)
		if b.failChild != nil {
			b.failChild(p.server, "token exchange failed: "+err.Error())
		}
		httpHTML(w, http.StatusBadGateway, "换取令牌失败", err.Error())
		return
	}

	b.logger.Printf("mcpbridge: oauth %q authorized; restarting server", p.server)
	if b.restartChild != nil {
		// Async: the restart re-runs the MCP handshake with the stored token;
		// don't block the browser response on it.
		go b.restartChild(p.server)
	}
	httpHTML(w, http.StatusOK, "授权成功",
		fmt.Sprintf("MCP server %q is now authorized — you can close this tab.", p.server))
}

func httpHTML(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>
<body style="font-family:system-ui;margin:4rem auto;max-width:32rem;text-align:center">
<h2>%s</h2><p style="color:#666">%s</p></body>`, title, title, detail)
}
