package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Every other test in this package swaps newRealChildTransport for a fake, so
// the two adapters that actually reach an upstream — realChildTransport's
// ListResources and ReadResource — are never executed. These tests spawn a
// real subprocess speaking real MCP over stdio and drive the bridge through
// its real HTTP transport, so the whole chain runs: JSON on a pipe, the
// initialize handshake, capability negotiation, and _meta surviving two
// serialisation boundaries rather than being handed between Go structs.
//
// The fixture server is this test binary re-executing itself: TestMain sees
// the env var and becomes an MCP server instead of running tests.

const fixtureEnv = "SANDRPOD_TEST_MCP_FIXTURE"

func TestMain(m *testing.M) {
	if os.Getenv(fixtureEnv) != "" {
		runFixtureServer(os.Getenv(fixtureEnv))
		return
	}
	os.Exit(m.Run())
}

// runFixtureServer is a genuine MCP server over stdio: one tool pointing at
// one MCP Apps interface resource, plus a binary resource to prove non-text
// contents survive. name distinguishes the two instances the collision test
// spawns.
func runFixtureServer(name string) {
	opts := []server.ServerOption{
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(false, true),
	}
	// The "paged" instance hands back small pages with a nextCursor, which is
	// how a real server with many resources behaves and what the bridge has
	// to keep following.
	if name == "paged" {
		opts = append(opts, server.WithPaginationLimit(10))
	}
	s := server.NewMCPServer("fixture-"+name, "1.0", opts...)

	if name == "paged" {
		for i := range pagedResourceCount {
			uri := fmt.Sprintf("doc://page/%03d", i)
			s.AddResource(
				mcp.Resource{URI: uri, Name: fmt.Sprintf("doc-%03d", i), MIMEType: "text/plain"},
				func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
					return []mcp.ResourceContents{mcp.TextResourceContents{
						URI: uri, MIMEType: "text/plain", Text: uri,
					}}, nil
				})
		}
		_ = server.ServeStdio(s)
		return
	}

	tool := mcp.Tool{
		Name:        "open_form",
		Description: "opens the form",
		InputSchema: mcp.ToolInputSchema{Type: "object"},
		Meta: &mcp.Meta{AdditionalFields: map[string]any{
			"ui": map[string]any{
				"resourceUri": "ui://form",
				"csp":         map[string]any{"connect-src": []any{"https://api.example.com"}},
				"permissions": []any{"clipboard-write"},
			},
		}},
	}
	s.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("called " + name), nil
	})

	s.AddResource(
		mcp.Resource{URI: "ui://form", Name: "form", MIMEType: "text/html;profile=mcp-app"},
		func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{
				URI:      "ui://form",
				MIMEType: "text/html;profile=mcp-app",
				Text:     "<h1>form from " + name + "</h1>",
				Meta: map[string]any{"ui": map[string]any{
					"csp":         map[string]any{"connect-src": []any{"https://api.example.com"}},
					"permissions": []any{"clipboard-write"},
					"domain":      "example.com",
				}},
			}}, nil
		})

	s.AddResource(
		mcp.Resource{URI: "asset://logo.png", Name: "logo", MIMEType: "image/png"},
		func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.BlobResourceContents{
				URI:      "asset://logo.png",
				MIMEType: "image/png",
				Blob:     "iVBORw0KGgo=", // base64, enough to prove the shape
			}}, nil
		})

	_ = server.ServeStdio(s)
}

// fixtureConfig writes an mcp.json whose entries re-exec this test binary.
func fixtureConfig(t *testing.T, names ...string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	entries := make([]string, 0, len(names))
	for _, n := range names {
		e, _ := json.Marshal(map[string]any{
			"command": self,
			"env":     map[string]string{fixtureEnv: n},
		})
		key, _ := json.Marshal(n)
		entries = append(entries, string(key)+":"+string(e))
	}
	return writeCfg(t, `{"mcpServers":{`+strings.Join(entries, ",")+`}}`)
}

// startReal starts a manager against real subprocesses — newRealChildTransport
// is deliberately NOT stubbed here.
func startReal(t *testing.T, cfgPath string) *ChildManager {
	t.Helper()
	m := NewManager(ManagerOptions{ConfigPath: cfgPath})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })

	for _, s := range m.Snapshot() {
		if s.State != string(StateReady) {
			t.Fatalf("child %s is %s: %s", s.Name, s.State, s.LastError)
		}
	}
	return m
}

// End to end over the bridge's real Streamable-HTTP surface, against a real
// upstream: the path a desktop MCP Apps host actually takes.
func TestResourcesEndToEndOverHTTP(t *testing.T) {
	m := startReal(t, fixtureConfig(t, "alpha"))

	srv := httptest.NewServer(NewHTTPHandler(m))
	t.Cleanup(srv.Close)

	c, err := client.NewStreamableHttpClient(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "host", Version: "1"}
	initRes, err := c.Initialize(ctx, initReq)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if initRes.Capabilities.Resources == nil {
		t.Fatal("no resources capability over HTTP")
	}
	// listChanged is the part that needs the explicit WithResourceCapabilities:
	// registering resources at all makes mcp-go declare the capability
	// implicitly, but with zero values, so a host would never learn that
	// resources appeared when a child came up — which for this bridge is the
	// normal case, not an edge one.
	if !initRes.Capabilities.Resources.ListChanged {
		t.Error("listChanged is false — hosts will not be told when a child's resources appear")
	}
	if initRes.Capabilities.Resources.Subscribe {
		t.Error("subscribe advertised, but the bridge does not proxy resource notifications")
	}

	// Step 1 of the MCP Apps load: read the interface URI off the tool.
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var uri string
	for _, tl := range tools.Tools {
		if !strings.HasSuffix(tl.Name, "open_form") {
			continue
		}
		blob, _ := json.Marshal(tl.Meta)
		var meta struct {
			UI struct {
				ResourceURI string `json:"resourceUri"`
				CSP         any    `json:"csp"`
				Permissions any    `json:"permissions"`
			} `json:"ui"`
		}
		if err := json.Unmarshal(blob, &meta); err != nil {
			t.Fatal(err)
		}
		uri = meta.UI.ResourceURI
		if meta.UI.CSP == nil || meta.UI.Permissions == nil {
			t.Errorf("tool _meta.ui lost siblings across the bridge: %s", blob)
		}
	}
	if uri != "ui://alpha/form" {
		t.Fatalf("tool points at %q, want ui://alpha/form", uri)
	}

	// Step 2: fetch it. This is the step the bridge could not serve at all.
	readReq := mcp.ReadResourceRequest{}
	readReq.Params.URI = uri
	got, err := c.ReadResource(ctx, readReq)
	if err != nil {
		t.Fatalf("resources/read %s: %v", uri, err)
	}
	tc, ok := got.Contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("contents[0] is %T, want TextResourceContents", got.Contents[0])
	}
	if !strings.Contains(tc.Text, "form from alpha") {
		t.Errorf("body = %q", tc.Text)
	}
	if tc.MIMEType != "text/html;profile=mcp-app" {
		t.Errorf("mimeType = %q", tc.MIMEType)
	}
	// The security declaration, after two JSON round trips (upstream stdio,
	// then the bridge's HTTP). This is the whole reason step 2 is mandatory.
	ui, _ := tc.Meta["ui"].(map[string]any)
	if ui == nil {
		t.Fatalf("_meta.ui gone: %+v", tc.Meta)
	}
	for _, k := range []string{"csp", "permissions", "domain"} {
		if ui[k] == nil {
			t.Errorf("_meta.ui.%s did not survive: %+v", ui, ui)
		}
	}
}

// Binary contents take a different branch than text and would otherwise be
// entirely untested.
func TestBlobResourceSurvivesTheBridge(t *testing.T) {
	m := startReal(t, fixtureConfig(t, "alpha"))

	got, err := m.DispatchResource(context.Background(), "asset://alpha/logo.png")
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	bc, ok := got.Contents[0].(mcp.BlobResourceContents)
	if !ok {
		t.Fatalf("contents[0] is %T, want BlobResourceContents", got.Contents[0])
	}
	if bc.Blob != "iVBORw0KGgo=" {
		t.Errorf("blob = %q", bc.Blob)
	}
	if bc.MIMEType != "image/png" {
		t.Errorf("mimeType = %q", bc.MIMEType)
	}
}

// The collision case against two real servers, both genuinely serving
// ui://form — the shape the namespacing exists for.
func TestTwoRealServersDoNotCollide(t *testing.T) {
	m := startReal(t, fixtureConfig(t, "alpha", "beta"))

	res := m.AggregatedResources()
	if len(res) != 4 { // ui://form + asset://logo.png, twice
		t.Fatalf("resources = %d, want 4: %+v", len(res), res)
	}

	for alias, marker := range map[string]string{"alpha": "form from alpha", "beta": "form from beta"} {
		uri := "ui://" + alias + "/form"
		got, err := m.DispatchResource(context.Background(), uri)
		if err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
		tc := got.Contents[0].(mcp.TextResourceContents)
		if !strings.Contains(tc.Text, marker) {
			t.Errorf("%s served %q, want the one saying %q", uri, tc.Text, marker)
		}
	}
}

// §6.6 in full: the index must survive every teardown path, not just Disable.
func TestURIIndexAcrossRestartAndReload(t *testing.T) {
	cfgPath := fixtureConfig(t, "alpha", "beta")
	m := startReal(t, cfgPath)
	ctx := context.Background()

	uris := func() map[string]uriEntry {
		m.mu.RLock()
		defer m.mu.RUnlock()
		out := make(map[string]uriEntry, len(m.uriIndex))
		maps.Copy(out, m.uriIndex)
		return out
	}

	if n := len(uris()); n != 4 {
		t.Fatalf("uriIndex = %d entries, want 4", n)
	}

	t.Run("restart rebuilds cleanly", func(t *testing.T) {
		if err := m.RestartServer(ctx, "alpha"); err != nil {
			t.Fatalf("restart: %v", err)
		}
		idx := uris()
		if len(idx) != 4 {
			t.Errorf("after restart uriIndex = %d, want 4: %+v", len(idx), idx)
		}
		if _, err := m.DispatchResource(ctx, "ui://alpha/form"); err != nil {
			t.Errorf("restarted child unreadable: %v", err)
		}
	})

	t.Run("reload drops the removed child", func(t *testing.T) {
		self, _ := os.Executable()
		body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
			"alpha": map[string]any{"command": self, "env": map[string]string{fixtureEnv: "alpha"}},
		}})
		if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := m.Reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		idx := uris()
		if _, stale := idx["ui://beta/form"]; stale {
			t.Error("uriIndex still routes to the child reload removed")
		}
		if _, alive := idx["ui://alpha/form"]; !alive {
			t.Errorf("reload dropped the surviving child's route: %+v", idx)
		}
		if _, err := m.DispatchResource(ctx, "ui://beta/form"); err == nil {
			t.Error("removed child's resource still readable")
		}
	})
}

// The allow/deny lists I added alongside the tool ones, against a real server.
func TestResourceFilterLists(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mk := func(t *testing.T, opts string) *ChildManager {
		body := `{"mcpServers":{"alpha":{"command":` + jsonStr(self) +
			`,"env":{"` + fixtureEnv + `":"alpha"},"sandrpod":{` + opts + `}}}}`
		return startReal(t, writeCfg(t, body))
	}

	t.Run("denylist removes one", func(t *testing.T) {
		m := mk(t, `"resource_denylist":["asset://logo.png"]`)
		got := uriSet(m.AggregatedResources())
		if _, blocked := got["asset://alpha/logo.png"]; blocked {
			t.Error("denylisted resource is still exposed")
		}
		if _, kept := got["ui://alpha/form"]; !kept {
			t.Error("denylist removed an unrelated resource")
		}
	})

	t.Run("allowlist keeps only that one", func(t *testing.T) {
		m := mk(t, `"resource_allowlist":["ui://form"]`)
		got := uriSet(m.AggregatedResources())
		if len(got) != 1 {
			t.Errorf("allowlist left %d resources, want 1: %v", len(got), got)
		}
		if _, kept := got["ui://alpha/form"]; !kept {
			t.Errorf("allowlisted resource missing: %v", got)
		}
	})

	t.Run("tool lists do not touch resources", func(t *testing.T) {
		// The trap the separate lists exist to avoid: a tool_allowlist must
		// not silently hide every resource.
		m := mk(t, `"tool_allowlist":["open_form"]`)
		if n := len(m.AggregatedResources()); n != 2 {
			t.Errorf("tool_allowlist changed the resource set to %d, want 2", n)
		}
	})
}

func uriSet(rs []mcp.Resource) map[string]bool {
	out := make(map[string]bool, len(rs))
	for _, r := range rs {
		out[r.URI] = true
	}
	return out
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// pagedResourceCount exceeds the fixture's 10-per-page limit several times
// over, so a bridge that reads only the first page is off by 115.
const pagedResourceCount = 125

// Pagination is part of the base protocol: a server MAY return a nextCursor
// and the caller MUST follow it. This asserts the bridge collects every page
// from an upstream that really paginates, and re-serves the whole set.
func TestUpstreamPaginationIsFollowed(t *testing.T) {
	m := startReal(t, fixtureConfig(t, "paged"))

	got := m.AggregatedResources()
	if len(got) != pagedResourceCount {
		t.Fatalf("aggregated %d resources, want %d — a page was dropped",
			len(got), pagedResourceCount)
	}

	// First, last and one in the middle must all resolve, so this is not
	// passing on a count alone.
	for _, i := range []int{0, pagedResourceCount / 2, pagedResourceCount - 1} {
		uri := fmt.Sprintf("doc://paged/page/%03d", i)
		res, err := m.DispatchResource(context.Background(), uri)
		if err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
		tc := res.Contents[0].(mcp.TextResourceContents)
		if want := fmt.Sprintf("doc://page/%03d", i); tc.Text != want {
			t.Errorf("%s returned %q, want %q", uri, tc.Text, want)
		}
	}

	// And the host sees all of them through the real transport.
	srv := httptest.NewServer(NewHTTPHandler(m))
	t.Cleanup(srv.Close)
	c, err := client.NewStreamableHttpClient(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "host", Version: "1"}
	if _, err := c.Initialize(ctx, req); err != nil {
		t.Fatal(err)
	}
	lr, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lr.Resources) != pagedResourceCount {
		t.Errorf("host saw %d resources, want %d", len(lr.Resources), pagedResourceCount)
	}
}
