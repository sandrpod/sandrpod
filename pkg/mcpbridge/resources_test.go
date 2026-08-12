package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/yosida95/uritemplate/v3"
)

// mkUIResource is an MCP Apps interface resource: HTML behind a ui:// URI,
// carrying the security declaration the host needs in _meta.
func mkUIResource(uri string) mcp.Resource {
	return mcp.Resource{
		URI:      uri,
		Name:     "form",
		MIMEType: "text/html;profile=mcp-app",
	}
}

// mkUITool is a tool that points at an interface resource the way SEP-1865
// specifies: _meta.ui.resourceUri, alongside the CSP declaration.
func mkUITool(name, resourceURI string) mcp.Tool {
	t := mkTool(name, "")
	t.Meta = &mcp.Meta{AdditionalFields: map[string]any{
		"ui": map[string]any{
			"resourceUri": resourceURI,
			"csp":         map[string]any{"connect-src": []any{"https://api.example.com"}},
		},
	}}
	return t
}

func uiHTML(uri, body string) *mcp.ReadResourceResult {
	return &mcp.ReadResourceResult{Contents: []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      uri,
			MIMEType: "text/html;profile=mcp-app",
			Text:     body,
			Meta: map[string]any{"ui": map[string]any{
				"csp": map[string]any{"connect-src": []any{"https://api.example.com"}},
			}},
		},
	}}
}

func startManager(t *testing.T, cfg string, fakes map[string]*fakeTransport) *ChildManager {
	t.Helper()
	withFakeTransport(t, fakes)
	m := NewManager(ManagerOptions{ConfigPath: writeCfg(t, cfg)})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m
}

// dialBridge drives the aggregator through a real MCP client over the
// in-process transport, so these tests exercise the protocol path a host
// actually takes rather than the Go methods underneath it.
func dialBridge(t *testing.T, m *ChildManager) *client.Client {
	t.Helper()
	c, err := client.NewInProcessClient(NewAggregatorServer(m))
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start client: %v", err)
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "test-host", Version: "1"}
	res, err := c.Initialize(context.Background(), req)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// §6.1 — the base protocol requires a server offering resources to
	// declare the capability. Without it a conforming client never sends
	// resources/read, whatever the bridge routes internally.
	if res.Capabilities.Resources == nil {
		t.Fatal("initialize did not declare the resources capability")
	}
	if !res.Capabilities.Resources.ListChanged {
		t.Error("listChanged should be true — the set changes as children come and go")
	}
	if res.Capabilities.Resources.Subscribe {
		t.Error("subscribe should be false — the bridge does not proxy resource notifications")
	}
	return c
}

// §6.1 + §6.2 over the wire: capability, list, then read with _meta intact.
func TestBridgeServesResourcesOverTheProtocol(t *testing.T) {
	m := startManager(t,
		`{"mcpServers":{"ui":{"command":"ui-bin"}}}`,
		map[string]*fakeTransport{"ui-bin": {
			tools:     []mcp.Tool{mkUITool("open", "ui://form")},
			resources: []mcp.Resource{mkUIResource("ui://form")},
			readResp:  uiHTML("ui://form", "<h1>hi</h1>"),
		}})
	c := dialBridge(t, m)
	ctx := context.Background()

	listed, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	if len(listed.Resources) != 1 || listed.Resources[0].URI != "ui://ui/form" {
		t.Fatalf("resources/list = %+v, want one ui://ui/form", listed.Resources)
	}

	readReq := mcp.ReadResourceRequest{}
	readReq.Params.URI = "ui://ui/form"
	got, err := c.ReadResource(ctx, readReq)
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	tc, ok := got.Contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("contents[0] is %T, want TextResourceContents", got.Contents[0])
	}
	if tc.MIMEType != "text/html;profile=mcp-app" {
		t.Errorf("mimeType = %q, want the mcp-app profile", tc.MIMEType)
	}
	// Serialised through a real transport, which is where a dropped _meta
	// would actually show up.
	ui, _ := tc.Meta["ui"].(map[string]any)
	if ui == nil || ui["csp"] == nil {
		t.Errorf("_meta.ui.csp did not survive the protocol round trip: %+v", tc.Meta)
	}

	// And the tool the host reads first must point at the URI that worked.
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(tools.Tools[0].Meta)
	var meta struct {
		UI struct {
			ResourceURI string `json:"resourceUri"`
		} `json:"ui"`
	}
	if err := json.Unmarshal(blob, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.UI.ResourceURI != "ui://ui/form" {
		t.Errorf("tools/list _meta.ui.resourceUri = %q, want ui://ui/form", meta.UI.ResourceURI)
	}
}

// §6.2 — list and read work end to end, and the resource-level _meta that
// carries the sandbox policy survives the round trip.
func TestResourceListAndReadPreserveMeta(t *testing.T) {
	f := &fakeTransport{
		tools:     []mcp.Tool{mkUITool("open", "ui://form")},
		resources: []mcp.Resource{mkUIResource("ui://form")},
		readResp:  uiHTML("ui://form", "<h1>hi</h1>"),
	}
	m := startManager(t, `{"mcpServers":{"ui":{"command":"ui-bin"}}}`,
		map[string]*fakeTransport{"ui-bin": f})

	res := m.AggregatedResources()
	if len(res) != 1 {
		t.Fatalf("aggregated resources = %d, want 1", len(res))
	}
	if res[0].URI != "ui://ui/form" {
		t.Errorf("bridged URI = %q, want ui://ui/form", res[0].URI)
	}

	got, err := m.DispatchResource(context.Background(), res[0].URI)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if f.lastReadURI != "ui://form" {
		t.Errorf("upstream saw %q, want the un-namespaced ui://form", f.lastReadURI)
	}
	tc, ok := got.Contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("contents[0] is %T, want TextResourceContents", got.Contents[0])
	}
	if tc.MIMEType != "text/html;profile=mcp-app" {
		t.Errorf("mimeType = %q", tc.MIMEType)
	}
	// The CSP is the whole point: strip it and the host has no sandbox policy
	// to apply to the HTML it just fetched.
	ui, _ := tc.Meta["ui"].(map[string]any)
	if ui == nil || ui["csp"] == nil {
		t.Errorf("_meta.ui.csp did not survive the bridge: %+v", tc.Meta)
	}
}

// §6.3 — the URI a host reads out of tools/list must be the one that works on
// resources/read. Rewriting the resource but not the pointer to it produces a
// tools/list that is confidently wrong.
func TestToolMetaResourceURIMatchesBridgedURI(t *testing.T) {
	m := startManager(t,
		`{"mcpServers":{"ui":{"command":"ui-bin"}}}`,
		map[string]*fakeTransport{"ui-bin": {
			tools:     []mcp.Tool{mkUITool("open", "ui://form")},
			resources: []mcp.Resource{mkUIResource("ui://form")},
			readResp:  uiHTML("ui://form", "<h1>hi</h1>"),
		}})

	tools := m.AggregatedTools()
	ui, _ := tools[0].Meta.AdditionalFields["ui"].(map[string]any)
	pointed, _ := ui["resourceUri"].(string)
	if pointed != "ui://ui/form" {
		t.Fatalf("_meta.ui.resourceUri = %q, want the bridged ui://ui/form", pointed)
	}
	if _, err := m.DispatchResource(context.Background(), pointed); err != nil {
		t.Errorf("the URI advertised in tools/list does not resolve: %v", err)
	}
	// csp must ride along untouched in the same _meta.
	if ui["csp"] == nil {
		t.Error("rewriting resourceUri dropped a sibling _meta.ui field")
	}
}

// The rewrite must not reach back into the child's own copy. Shallow-cloning
// the Tool shares *Meta and its map, so an in-place write corrupts the
// upstream record for every later caller — and the second AggregatedTools
// would then namespace an already-namespaced URI.
func TestToolMetaRewriteDoesNotMutateChild(t *testing.T) {
	m := startManager(t,
		`{"mcpServers":{"ui":{"command":"ui-bin"}}}`,
		map[string]*fakeTransport{"ui-bin": {
			tools:     []mcp.Tool{mkUITool("open", "ui://form")},
			resources: []mcp.Resource{mkUIResource("ui://form")},
		}})

	first := m.AggregatedTools()
	second := m.AggregatedTools()

	for i, tools := range [][]mcp.Tool{first, second} {
		ui, _ := tools[0].Meta.AdditionalFields["ui"].(map[string]any)
		if got := ui["resourceUri"]; got != "ui://ui/form" {
			t.Fatalf("call %d: resourceUri = %v, want ui://ui/form (double-namespaced?)", i+1, got)
		}
	}

	m.mu.RLock()
	child := m.children["ui"]
	m.mu.RUnlock()
	orig, _ := child.Tools()[0].Meta.AdditionalFields["ui"].(map[string]any)
	if got := orig["resourceUri"]; got != "ui://form" {
		t.Errorf("child's own copy was mutated to %v; must stay ui://form", got)
	}
}

// §6.4 — the collision that single-server testing cannot see. Two servers both
// exposing ui://form must stay distinguishable, and each must read back its
// own HTML.
func TestTwoServersExposingTheSameURI(t *testing.T) {
	a := &fakeTransport{
		tools:     []mcp.Tool{mkUITool("open", "ui://form")},
		resources: []mcp.Resource{mkUIResource("ui://form")},
		readResp:  uiHTML("ui://form", "<h1>from-a</h1>"),
	}
	b := &fakeTransport{
		tools:     []mcp.Tool{mkUITool("open", "ui://form")},
		resources: []mcp.Resource{mkUIResource("ui://form")},
		readResp:  uiHTML("ui://form", "<h1>from-b</h1>"),
	}
	m := startManager(t,
		`{"mcpServers":{"alpha":{"command":"a-bin"},"beta":{"command":"b-bin"}}}`,
		map[string]*fakeTransport{"a-bin": a, "b-bin": b})

	res := m.AggregatedResources()
	if len(res) != 2 {
		t.Fatalf("aggregated resources = %d, want 2", len(res))
	}
	if res[0].URI == res[1].URI {
		t.Fatalf("both servers landed on %q — namespacing failed", res[0].URI)
	}

	want := map[string]string{"ui://alpha/form": "from-a", "ui://beta/form": "from-b"}
	for uri, marker := range want {
		got, err := m.DispatchResource(context.Background(), uri)
		if err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
		tc := got.Contents[0].(mcp.TextResourceContents)
		if !strings.Contains(tc.Text, marker) {
			t.Errorf("%s returned %q, want the one containing %q", uri, tc.Text, marker)
		}
	}

	// And each tool must point at its own server's resource.
	for _, tool := range m.AggregatedTools() {
		ui, _ := tool.Meta.AdditionalFields["ui"].(map[string]any)
		uri, _ := ui["resourceUri"].(string)
		if _, ok := want[uri]; !ok {
			t.Errorf("tool %s points at %q, which is not one of the bridged URIs", tool.Name, uri)
		}
	}
}

// §6.5 — the overwhelming majority of MCP servers expose no resources. They
// must be untouched, and in particular must not be marked failed.
func TestServerWithoutResourcesIsUnaffected(t *testing.T) {
	t.Run("no capability declared", func(t *testing.T) {
		m := startManager(t, `{"mcpServers":{"plain":{"command":"plain-bin"}}}`,
			map[string]*fakeTransport{"plain-bin": {tools: []mcp.Tool{mkTool("t", "")}}})

		snap := m.Snapshot()
		if snap[0].State != string(StateReady) {
			t.Errorf("state = %s, want ready", snap[0].State)
		}
		if snap[0].ResourceCount != 0 {
			t.Errorf("resource count = %d, want 0", snap[0].ResourceCount)
		}
		if n := len(m.AggregatedResources()); n != 0 {
			t.Errorf("aggregated resources = %d, want 0", n)
		}
	})

	// A server that advertises resources but fails the list is degraded to
	// "no resources", never to failed — its tools are still perfectly good.
	t.Run("capability declared but list fails", func(t *testing.T) {
		m := startManager(t, `{"mcpServers":{"broken":{"command":"broken-bin"}}}`,
			map[string]*fakeTransport{"broken-bin": {
				tools:      []mcp.Tool{mkTool("t", "")},
				listResErr: errors.New("-32601 method not found"),
			}})

		snap := m.Snapshot()
		if snap[0].State != string(StateReady) {
			t.Fatalf("state = %s, want ready — a resources/list failure must not fail the child", snap[0].State)
		}
		if n := len(m.AggregatedTools()); n != 1 {
			t.Errorf("tools = %d, want 1 — tools must keep working", n)
		}
	})
}

// §6.6 — a stopped or restarted child must leave no dangling route behind.
func TestURIIndexHasNoDanglingEntries(t *testing.T) {
	mk := func() *fakeTransport {
		return &fakeTransport{
			tools:     []mcp.Tool{mkUITool("open", "ui://form")},
			resources: []mcp.Resource{mkUIResource("ui://form")},
			readResp:  uiHTML("ui://form", "<h1>x</h1>"),
		}
	}
	m := startManager(t,
		`{"mcpServers":{"alpha":{"command":"a-bin"},"beta":{"command":"b-bin"}}}`,
		map[string]*fakeTransport{"a-bin": mk(), "b-bin": mk()})

	if n := len(m.AggregatedResources()); n != 2 {
		t.Fatalf("resources = %d, want 2", n)
	}

	if err := m.DisableServer(context.Background(), "beta"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	m.mu.RLock()
	idx := make(map[string]uriEntry, len(m.uriIndex))
	for k, v := range m.uriIndex {
		idx[k] = v
	}
	m.mu.RUnlock()

	if _, stale := idx["ui://beta/form"]; stale {
		t.Error("uriIndex still routes to the disabled child")
	}
	if _, alive := idx["ui://alpha/form"]; !alive {
		t.Error("disabling one child dropped the other's route")
	}
	if _, err := m.DispatchResource(context.Background(), "ui://beta/form"); err == nil {
		t.Error("reading a disabled child's resource should fail, not hang or succeed")
	}
	if _, err := m.DispatchResource(context.Background(), "ui://alpha/form"); err != nil {
		t.Errorf("surviving child unreadable after the other was disabled: %v", err)
	}
}

// The gate sees resource reads, and a denial blocks the upstream call rather
// than merely being logged.
func TestResourceReadPassesThroughPermissionGate(t *testing.T) {
	f := &fakeTransport{
		tools:     []mcp.Tool{mkUITool("open", "ui://form")},
		resources: []mcp.Resource{mkUIResource("ui://form")},
		readResp:  uiHTML("ui://form", "<h1>x</h1>"),
	}
	withFakeTransport(t, map[string]*fakeTransport{"ui-bin": f})

	gate := &recordingGate{decision: DecisionDeny}
	audit := &recordingAudit{}
	m := NewManager(ManagerOptions{
		ConfigPath: writeCfg(t, `{"mcpServers":{"ui":{"command":"ui-bin"}}}`),
		Permission: gate,
		Audit:      audit,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })

	if _, err := m.DispatchResource(context.Background(), "ui://ui/form"); err == nil {
		t.Fatal("denied read returned no error")
	}
	if f.lastReadURI != "" {
		t.Errorf("upstream was reached despite denial (read %q)", f.lastReadURI)
	}

	var seen *PermissionEvent
	ev := gate.seen()
	for i := range ev {
		if ev[i].Source == "mcp.resource" {
			seen = &ev[i]
		}
	}
	if seen == nil {
		t.Fatalf("gate never saw an mcp.resource event; got %+v", ev)
	}
	if seen.Resource != "ui://form" {
		t.Errorf("gate saw Resource=%q, want the un-namespaced ui://form", seen.Resource)
	}
	if seen.Tool != "" {
		t.Errorf("Resource events must not populate Tool (got %q)", seen.Tool)
	}

	var denial *AuditEvent
	av := audit.seen()
	for i := range av {
		if av[i].Source == "mcp.resource" {
			denial = &av[i]
		}
	}
	if denial == nil || denial.Decision != DecisionDeny {
		t.Errorf("denial not audited: %+v", av)
	}
}

func TestFullyQualifiedURI(t *testing.T) {
	tests := []struct {
		name, alias, uri, want string
	}{
		{"ui scheme", "notion", "ui://form", "ui://notion/form"},
		{"nested path", "notion", "ui://form/edit", "ui://notion/form/edit"},
		{"file scheme", "fs", "file:///etc/hosts", "file://fs//etc/hosts"},
		{"opaque scheme", "x", "urn:ietf:rfc:7231", "sandrpod://x/urn:ietf:rfc:7231"},
		{"no scheme at all", "x", "bare", "sandrpod://x/bare"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fullyQualifiedURI(tc.alias, tc.uri); got != tc.want {
				t.Errorf("fullyQualifiedURI(%q, %q) = %q, want %q", tc.alias, tc.uri, got, tc.want)
			}
		})
	}

	// Long aliases are hashed the same way tool names are, and the result
	// must still differ per alias or two servers would merge.
	long1, long2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if fullyQualifiedURI(long1, "ui://f") == fullyQualifiedURI(long2, "ui://f") {
		t.Error("two long aliases collapsed to the same URI")
	}
}

// recordingGate allows everything except the source under test. A blanket
// deny would stop the child spawning, so there would be no resource to read
// and the test would pass for the wrong reason.
type recordingGate struct {
	mu       sync.Mutex
	decision Decision
	deny     string
	events   []PermissionEvent
}

func (g *recordingGate) Check(_ context.Context, e PermissionEvent) (Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, e)
	target := g.deny
	if target == "" {
		target = "mcp.resource"
	}
	if e.Source == target {
		return g.decision, nil
	}
	return DecisionAllow, nil
}

func (g *recordingGate) seen() []PermissionEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]PermissionEvent(nil), g.events...)
}

func (r *recordingAudit) seen() []AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AuditEvent(nil), r.events...)
}

// The template fallback must not become "this child has templates, so any URI
// goes". Tested against a fake that serves anything: with a real upstream the
// server does its own template matching and would mask a broken guard here,
// which is exactly how the first version of this test passed while asserting
// nothing.
func TestResourceTemplateGuard(t *testing.T) {
	tpl, err := uritemplate.New("doc://{section}/body")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTransport{
		tools:     []mcp.Tool{mkTool("t", "")},
		resources: []mcp.Resource{},
		templates: []mcp.ResourceTemplate{{
			Name:        "doc",
			URITemplate: &mcp.URITemplate{Template: tpl},
		}},
		// Serves whatever it is asked for — so anything that gets through is
		// the bridge's decision, not the upstream's.
		readResp: &mcp.ReadResourceResult{Contents: []mcp.ResourceContents{
			mcp.TextResourceContents{URI: "any", MIMEType: "text/plain", Text: "served"},
		}},
	}
	m := startManager(t, `{"mcpServers":{"alpha":{"command":"a-bin"}}}`,
		map[string]*fakeTransport{"a-bin": f})
	ctx := context.Background()

	if _, err := m.DispatchResource(ctx, "doc://alpha/intro/body"); err != nil {
		t.Fatalf("a legitimate expansion was rejected: %v", err)
	}
	if f.lastReadURI != "doc://intro/body" {
		t.Errorf("upstream saw %q, want the un-namespaced doc://intro/body", f.lastReadURI)
	}

	for _, bad := range []string{
		"doc://alpha/intro/body/extra", // more segments than the template
		"doc://alpha/intro",            // fewer
		"secret://alpha/etc/passwd",    // different scheme entirely
	} {
		f.lastReadURI = ""
		if _, err := m.DispatchResource(ctx, bad); err == nil {
			t.Errorf("%s was served, but matches no declared template", bad)
		}
		if f.lastReadURI != "" {
			t.Errorf("%s reached the upstream as %q — the guard let it past", bad, f.lastReadURI)
		}
	}
}
