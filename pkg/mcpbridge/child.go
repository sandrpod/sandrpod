package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// ChildState is the lifecycle state of a single stdio child.
type ChildState string

const (
	StateStarting   ChildState = "starting"
	StateReady      ChildState = "ready"
	StateFailed     ChildState = "failed"
	StateRestarting ChildState = "restarting"
	StateStopped    ChildState = "stopped"
)

// childTransport is the subset of the mcp-go client we actually use. Defined
// as an interface so tests can plug in a fake without spawning a process.
type childTransport interface {
	Start(ctx context.Context) error
	Initialize(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error)
	ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error)
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	// Ping is used by the supervisor goroutine to detect dead children
	// (stdio child has crashed or hung). MCP defines ping as a no-op
	// JSON-RPC method; if the child can't answer, it's gone.
	Ping(ctx context.Context) error
	Close() error
}

// realChildTransport adapts *client.Client to childTransport.
type realChildTransport struct{ c *client.Client }

func (r *realChildTransport) Start(ctx context.Context) error { return r.c.Start(ctx) }
func (r *realChildTransport) Initialize(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	return r.c.Initialize(ctx, req)
}
func (r *realChildTransport) ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	return r.c.ListTools(ctx, req)
}
func (r *realChildTransport) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return r.c.CallTool(ctx, req)
}
func (r *realChildTransport) Ping(ctx context.Context) error { return r.c.Ping(ctx) }
func (r *realChildTransport) Close() error                   { return r.c.Close() }

// newRealChildTransport builds the MCP client for a server entry: a spawned
// stdio subprocess (Command set) or an HTTP connection (URL set).
// Kept as a package var so tests can override it.
var newRealChildTransport = func(cfg ServerConfig) (childTransport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.IsHTTP() {
		return newHTTPChildTransport(cfg)
	}

	envSlice := make([]string, 0, len(cfg.Env))
	for k, v := range cfg.Env {
		envSlice = append(envSlice, k+"="+v)
	}
	sort.Strings(envSlice) // deterministic ordering for the child process env

	cli, err := client.NewStdioMCPClient(cfg.Command, envSlice, cfg.Args...)
	if err != nil {
		return nil, fmt.Errorf("spawn stdio child: %w", err)
	}
	return &realChildTransport{c: cli}, nil
}

// newHTTPChildTransport connects to a remote MCP server over Streamable-HTTP
// (default) or SSE. Header values support ${ENV} / $ENV expansion so secrets
// can be referenced from the environment rather than stored in mcp.json.
func newHTTPChildTransport(cfg ServerConfig) (childTransport, error) {
	url := expandEnv(cfg.URL, cfg.Env)
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = expandEnv(v, cfg.Env)
	}

	var (
		cli *client.Client
		err error
	)
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "sse":
		var opts []transport.ClientOption
		if len(headers) > 0 {
			opts = append(opts, transport.WithHeaders(headers))
		}
		cli, err = client.NewSSEMCPClient(url, opts...)
	case "", "http", "streamable-http", "streamable_http", "streamablehttp":
		var opts []transport.StreamableHTTPCOption
		if len(headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(headers))
		}
		cli, err = client.NewStreamableHttpClient(url, opts...)
	default:
		return nil, fmt.Errorf("unknown transport type %q (use streamable-http|http|sse)", cfg.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("create http MCP client: %w", err)
	}
	return &realChildTransport{c: cli}, nil
}

// expandEnv expands ${VAR} / $VAR in s, preferring the per-server env map and
// falling back to the process environment.
func expandEnv(s string, env map[string]string) string {
	return os.Expand(s, func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return os.Getenv(k)
	})
}

// Child wraps a single stdio MCP server subprocess.
type Child struct {
	Name  string // mcp.json key (e.g. "github")
	Alias string // namespace prefix (defaults to Name)
	Cfg   ServerConfig

	mu           sync.RWMutex
	transport    childTransport
	tools        []mcp.Tool
	state        ChildState
	lastError    string
	startedAt    time.Time
	restarts     int
	restartTimes []time.Time // sliding window for MaxRestartPerMin
	configHash   string      // set by manager for Reload diff

	// inFlight tracks tools/call invocations currently waiting on the
	// child. Used by Stop to drain gracefully — without it, SIGTERM
	// arriving mid-call leaves the caller hanging on an EOF read after
	// the subprocess is killed.
	inFlight sync.WaitGroup
}

func newChild(name string, cfg ServerConfig) *Child {
	return &Child{
		Name:  name,
		Alias: cfg.AliasOr(name),
		Cfg:   cfg,
		state: StateStopped,
	}
}

// Start spawns the child process, performs the MCP handshake, and caches the
// tools/list result. Filters via tool_allowlist / tool_denylist are applied
// before caching, so the bridge never exposes filtered tools.
func (c *Child) Start(ctx context.Context) error {
	c.mu.Lock()
	c.state = StateStarting
	c.lastError = ""
	c.mu.Unlock()

	t, err := newRealChildTransport(c.Cfg)
	if err != nil {
		c.setFailed(err)
		return err
	}

	startupTimeout := 30 * time.Second
	if c.Cfg.Sandrpod != nil && c.Cfg.Sandrpod.StartupTimeoutSec > 0 {
		startupTimeout = time.Duration(c.Cfg.Sandrpod.StartupTimeoutSec) * time.Second
	}
	hsCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	if err := t.Start(hsCtx); err != nil {
		_ = t.Close()
		c.setFailed(fmt.Errorf("start transport: %w", err))
		return err
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "sandrpod-mcp-bridge",
		Version: "0.1.0",
	}
	if _, err := t.Initialize(hsCtx, initReq); err != nil {
		_ = t.Close()
		c.setFailed(fmt.Errorf("initialize: %w", err))
		return err
	}

	listResp, err := t.ListTools(hsCtx, mcp.ListToolsRequest{})
	if err != nil {
		_ = t.Close()
		c.setFailed(fmt.Errorf("tools/list: %w", err))
		return err
	}

	tools := filterTools(listResp.Tools, c.Cfg.Sandrpod)

	c.mu.Lock()
	c.transport = t
	c.tools = tools
	c.state = StateReady
	c.startedAt = time.Now()
	c.mu.Unlock()

	return nil
}

// Stop closes the underlying transport (which SIGTERMs the subprocess).
// Safe to call multiple times.
func (c *Child) Stop(_ context.Context) error {
	c.mu.Lock()
	t := c.transport
	c.transport = nil
	c.tools = nil
	c.state = StateStopped
	c.mu.Unlock()
	if t == nil {
		return nil
	}
	return t.Close()
}

// Tools returns a copy of the cached tools slice. Safe for concurrent use.
func (c *Child) Tools() []mcp.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]mcp.Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// State returns the current lifecycle state.
func (c *Child) State() ChildState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// LastError returns the most recent failure reason, if any.
func (c *Child) LastError() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastError
}

// CallTool proxies a call to the child. Returns an error if the child is not
// ready or the tool was filtered out.
//
// The in-flight WaitGroup is incremented while the call is outstanding so
// Stop / WaitDrain can wait for it to complete before killing the child.
func (c *Child) CallTool(ctx context.Context, name string, args any) (*mcp.CallToolResult, error) {
	c.mu.RLock()
	t := c.transport
	state := c.state
	tools := c.tools
	c.mu.RUnlock()

	if state != StateReady || t == nil {
		return nil, fmt.Errorf("child %s not ready (state=%s)", c.Name, state)
	}
	if !toolKnown(tools, name) {
		return nil, fmt.Errorf("tool %s not exposed by child %s", name, c.Name)
	}

	c.inFlight.Add(1)
	defer c.inFlight.Done()

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return t.CallTool(ctx, req)
}

// WaitDrain blocks until all in-flight CallTool invocations on this child
// have returned, or ctx is cancelled. Returns true on clean drain, false
// if the context deadline fired first.
func (c *Child) WaitDrain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		c.inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Child) setFailed(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateFailed
	if err != nil {
		c.lastError = err.Error()
	}
}

// setFailedReason marks the child failed with a free-form reason string.
// Used when the cause isn't a wrapped error (e.g. permission denial).
func (c *Child) setFailedReason(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateFailed
	c.lastError = reason
}

// Ping probes the child via the MCP ping method. Returns error if the
// child is not ready or the ping fails (transport dead / timeout).
func (c *Child) Ping(ctx context.Context) error {
	c.mu.RLock()
	t := c.transport
	state := c.state
	c.mu.RUnlock()
	if state != StateReady || t == nil {
		return ErrChildNotReady
	}
	return t.Ping(ctx)
}

// recordRestartAttempt returns true if a restart is allowed under the
// per-minute rate limit and bumps the sliding-window counter; false if
// the limit was already hit.
func (c *Child) recordRestartAttempt(maxPerMin int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	kept := c.restartTimes[:0]
	for _, ts := range c.restartTimes {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	c.restartTimes = kept
	if len(c.restartTimes) >= maxPerMin {
		return false
	}
	c.restartTimes = append(c.restartTimes, now)
	return true
}

func toolKnown(tools []mcp.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func filterTools(in []mcp.Tool, opts *SandrpodOpts) []mcp.Tool {
	if opts == nil {
		return in
	}
	allow := opts.ToolAllowlist
	deny := opts.ToolDenylist
	if len(allow) == 0 && len(deny) == 0 {
		return in
	}
	out := make([]mcp.Tool, 0, len(in))
	for _, t := range in {
		if len(allow) > 0 && !slices.Contains(allow, t.Name) {
			continue
		}
		if slices.Contains(deny, t.Name) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ErrChildNotReady is returned by methods called on a non-ready child.
var ErrChildNotReady = errors.New("mcp child not ready")
