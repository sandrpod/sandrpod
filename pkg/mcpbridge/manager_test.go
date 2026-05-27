package mcpbridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// fakeTransport satisfies childTransport without spawning a subprocess.
type fakeTransport struct {
	mu        sync.Mutex
	tools     []mcp.Tool
	closed    bool
	lastCall  string
	lastArgs  any
	callResp  *mcp.CallToolResult
	callErr   error
}

func (f *fakeTransport) Start(context.Context) error { return nil }
func (f *fakeTransport) Initialize(context.Context, mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	return &mcp.InitializeResult{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ServerInfo:      mcp.Implementation{Name: "fake", Version: "1"},
	}, nil
}
func (f *fakeTransport) ListTools(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: f.tools}, nil
}
func (f *fakeTransport) CallTool(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	f.lastCall = req.Params.Name
	f.lastArgs = req.Params.Arguments
	f.mu.Unlock()
	if f.callErr != nil {
		return nil, f.callErr
	}
	return f.callResp, nil
}
func (f *fakeTransport) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errFakeClosed
	}
	return nil
}
func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

var errFakeClosed = &fakeErr{"closed"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }

func mkTool(name, desc string) mcp.Tool {
	return mcp.Tool{
		Name:        name,
		Description: desc,
		InputSchema: mcp.ToolInputSchema{Type: "object"},
	}
}

func withFakeTransport(t *testing.T, fakes map[string]*fakeTransport) {
	t.Helper()
	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) {
		// Key by command for simplicity in tests.
		f, ok := fakes[cfg.Command]
		if !ok {
			t.Fatalf("no fake registered for command %q", cfg.Command)
		}
		return f, nil
	}
	t.Cleanup(func() { newRealChildTransport = prev })
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManager_StartAggregatesAndDispatches(t *testing.T) {
	ghTools := []mcp.Tool{mkTool("list_issues", "List issues"), mkTool("create_pr", "Open PR")}
	jrTools := []mcp.Tool{mkTool("create_issue", "New JIRA")}
	withFakeTransport(t, map[string]*fakeTransport{
		"gh-bin":   {tools: ghTools, callResp: &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "ok"}}}},
		"jira-bin": {tools: jrTools, callResp: &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "ok"}}}},
	})

	cfgPath := writeCfg(t, `{
	  "mcpServers": {
	    "github": {"command": "gh-bin"},
	    "jira":   {"command": "jira-bin"}
	  }
	}`)

	m := NewManager(ManagerOptions{ConfigPath: cfgPath})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	tools := m.AggregatedTools()
	if len(tools) != 3 {
		t.Fatalf("tools count = %d, want 3", len(tools))
	}
	// Tool names must be deterministically prefixed.
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
		if !strings.HasPrefix(tl.Description, "[") {
			t.Errorf("description not tagged with alias: %q", tl.Description)
		}
	}
	for _, want := range []string{"github__list_issues", "github__create_pr", "jira__create_issue"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}

	res, err := m.Dispatch(context.Background(), "github__list_issues", map[string]any{"state": "open"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("empty result")
	}
}

func TestManager_FilteredTools(t *testing.T) {
	tools := []mcp.Tool{mkTool("safe", ""), mkTool("danger", "")}
	withFakeTransport(t, map[string]*fakeTransport{"gh-bin": {tools: tools}})
	cfgPath := writeCfg(t, `{
	  "mcpServers": {
	    "github": {"command": "gh-bin", "sandrpod": {"tool_denylist": ["danger"]}}
	  }
	}`)
	m := NewManager(ManagerOptions{ConfigPath: cfgPath})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	got := m.AggregatedTools()
	if len(got) != 1 || got[0].Name != "github__safe" {
		t.Fatalf("expected only github__safe, got %+v", got)
	}
}

func TestManager_DisabledSkipped(t *testing.T) {
	withFakeTransport(t, map[string]*fakeTransport{})
	cfgPath := writeCfg(t, `{
	  "mcpServers": {
	    "off": {"command": "never-run", "sandrpod": {"enabled": false}}
	  }
	}`)
	m := NewManager(ManagerOptions{ConfigPath: cfgPath})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshot()) != 0 {
		t.Errorf("disabled server should not appear in snapshot")
	}
}

func TestFullyQualifiedName_truncatesLongAlias(t *testing.T) {
	short := fullyQualifiedName("github", "list_issues")
	if short != "github__list_issues" {
		t.Errorf("short fq = %q", short)
	}
	long := fullyQualifiedName("very_long_namespace_alias_here", "do_thing")
	if !strings.Contains(long, "__do_thing") {
		t.Errorf("missing tool tail: %q", long)
	}
	alias, tool, ok := SplitFQName(long)
	if !ok {
		t.Fatalf("SplitFQName failed")
	}
	if len(alias) > aliasMaxLen {
		t.Errorf("alias not truncated: len=%d", len(alias))
	}
	if tool != "do_thing" {
		t.Errorf("tool round-trip = %q", tool)
	}
	// Distinct long aliases yield distinct truncated forms.
	other := fullyQualifiedName("very_long_namespace_alias_other", "do_thing")
	if other == long {
		t.Errorf("hash suffix did not disambiguate: %q == %q", other, long)
	}
}

type denyGate struct{}

func (denyGate) Check(context.Context, PermissionEvent) (Decision, error) { return DecisionDeny, nil }

func TestManager_PermissionDenyOnSpawn(t *testing.T) {
	withFakeTransport(t, map[string]*fakeTransport{"x": {tools: []mcp.Tool{mkTool("t", "")}}})
	cfgPath := writeCfg(t, `{"mcpServers":{"foo":{"command":"x"}}}`)
	m := NewManager(ManagerOptions{ConfigPath: cfgPath, Permission: denyGate{}})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.AggregatedTools()) != 0 {
		t.Errorf("spawn-denied child should expose no tools")
	}
}

type recordingAudit struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *recordingAudit) Record(e AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func TestManager_AuditOnCall(t *testing.T) {
	withFakeTransport(t, map[string]*fakeTransport{
		"x": {tools: []mcp.Tool{mkTool("ping", "")}, callResp: &mcp.CallToolResult{}},
	})
	cfgPath := writeCfg(t, `{"mcpServers":{"foo":{"command":"x"}}}`)
	rec := &recordingAudit{}
	m := NewManager(ManagerOptions{ConfigPath: cfgPath, Audit: rec})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Dispatch(context.Background(), "foo__ping", nil); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var sawCall bool
	for _, e := range rec.events {
		if e.Source == "mcp.call" && e.Tool == "ping" {
			sawCall = true
			if e.ResultStatus != "ok" {
				t.Errorf("status = %q, want ok", e.ResultStatus)
			}
		}
	}
	if !sawCall {
		t.Errorf("no mcp.call audit event recorded: %+v", rec.events)
	}
}
