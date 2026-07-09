package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Goal of these benchmarks: confirm the architecture sustains the
// throughput the design doc targets (10 servers × 100 calls/s) without
// quietly degrading on lock contention or aggregator overhead.
//
// We use a near-zero-latency fake transport so the benchmark measures
// the BRIDGE'S overhead, not the upstream MCP server's. Real npx
// children dominate by orders of magnitude.

// fastTransport returns immediately. Tools call returns a tiny result.
type fastTransport struct {
	tools []mcp.Tool
}

func (f *fastTransport) Start(context.Context) error { return nil }
func (f *fastTransport) Initialize(context.Context, mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	return &mcp.InitializeResult{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ServerInfo:      mcp.Implementation{Name: "fast", Version: "1"},
	}, nil
}
func (f *fastTransport) ListTools(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: f.tools}, nil
}
func (f *fastTransport) CallTool(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "ok"}}}, nil
}
func (f *fastTransport) Ping(context.Context) error { return nil }
func (f *fastTransport) Close() error               { return nil }

func setupBenchManager(b *testing.B, numServers, toolsPerServer int) *ChildManager {
	b.Helper()
	servers := make(map[string]ServerConfig, numServers)
	cmds := map[string]bool{}
	for i := 0; i < numServers; i++ {
		name := fmt.Sprintf("srv%02d", i)
		cmd := name + "-bin"
		servers[name] = ServerConfig{Command: cmd}
		cmds[cmd] = true
	}

	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) {
		tools := make([]mcp.Tool, toolsPerServer)
		for j := range tools {
			tools[j] = mcp.Tool{
				Name:        fmt.Sprintf("tool%02d", j),
				Description: "fast",
				InputSchema: mcp.ToolInputSchema{Type: "object"},
			}
		}
		return &fastTransport{tools: tools}, nil
	}
	b.Cleanup(func() { newRealChildTransport = prev })

	// Write a temp config file.
	body, _ := json.Marshal(Config{McpServers: servers})
	dir := b.TempDir()
	cfgPath := dir + "/mcp.json"
	if err := writeFile(cfgPath, body); err != nil {
		b.Fatal(err)
	}

	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		SupervisorInterval: time.Hour, // disable supervisor noise during bench
	})
	if err := m.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

// BenchmarkDispatch_10Servers measures call throughput against 10 servers
// of 5 tools each. The b.RunParallel pattern simulates many concurrent
// callers (which is how real HTTP clients hit /mcp).
func BenchmarkDispatch_10Servers(b *testing.B) {
	const (
		numServers     = 10
		toolsPerServer = 5
	)
	m := setupBenchManager(b, numServers, toolsPerServer)

	// Pre-compute the fqn pool the parallel goroutines round-robin.
	fqns := make([]string, 0, numServers*toolsPerServer)
	for _, t := range m.AggregatedTools() {
		fqns = append(fqns, t.Name)
	}
	if len(fqns) != numServers*toolsPerServer {
		b.Fatalf("expected %d tools, got %d", numServers*toolsPerServer, len(fqns))
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			fqn := fqns[i%len(fqns)]
			i++
			if _, err := m.Dispatch(context.Background(), fqn, nil); err != nil {
				b.Fatalf("Dispatch: %v", err)
			}
		}
	})
}

// BenchmarkAggregatedTools measures the cost of building the full tool
// list (called once per Streamable HTTP request's tools/list).
func BenchmarkAggregatedTools(b *testing.B) {
	m := setupBenchManager(b, 10, 5)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.AggregatedTools()
	}
}

// TestLoad_10ServersX100Per acts as a sanity load test that runs in CI
// (unlike benchmarks). It validates the design doc's 10 servers × 100
// calls/s target by firing 1000 calls across 10 fake servers and
// asserting throughput plus p99 latency stay reasonable.
//
// Marked as a Test (not Benchmark) so `go test ./...` exercises it on
// every CI run — the goal is regression detection, not headline TPS.
func TestLoad_10ServersX100PerSec(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped in -short")
	}

	const (
		numServers     = 10
		toolsPerServer = 5
		totalCalls     = 1000
		concurrency    = 20
	)

	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) {
		tools := make([]mcp.Tool, toolsPerServer)
		for j := range tools {
			tools[j] = mcp.Tool{Name: fmt.Sprintf("tool%02d", j), InputSchema: mcp.ToolInputSchema{Type: "object"}}
		}
		return &fastTransport{tools: tools}, nil
	}
	t.Cleanup(func() { newRealChildTransport = prev })

	servers := map[string]ServerConfig{}
	for i := 0; i < numServers; i++ {
		name := fmt.Sprintf("srv%02d", i)
		servers[name] = ServerConfig{Command: name + "-bin"}
	}
	body, _ := json.Marshal(Config{McpServers: servers})
	dir := t.TempDir()
	cfgPath := dir + "/mcp.json"
	if err := writeFile(cfgPath, body); err != nil {
		t.Fatal(err)
	}

	m := NewManager(ManagerOptions{ConfigPath: cfgPath, SupervisorInterval: time.Hour})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	fqns := []string{}
	for _, tl := range m.AggregatedTools() {
		fqns = append(fqns, tl.Name)
	}

	// Drive load through the actual HTTP handler too, so we measure
	// the full request path (parse → mcp-go → aggregator → child).
	srv := httptest.NewServer(NewHTTPHandler(m))
	defer srv.Close()

	// Initialize once to grab a session id.
	sid := initSession(t, srv.URL)

	latencies := make([]time.Duration, totalCalls)
	var idx atomic.Int32
	sem := make(chan struct{}, concurrency)

	start := time.Now()
	done := make(chan struct{})
	for i := 0; i < totalCalls; i++ {
		sem <- struct{}{}
		go func(callIdx int) {
			defer func() { <-sem }()
			defer func() {
				if int(idx.Add(1)) == totalCalls {
					close(done)
				}
			}()
			fqn := fqns[callIdx%len(fqns)]
			t0 := time.Now()
			callTool(t, srv.URL, sid, callIdx+2, fqn)
			latencies[callIdx] = time.Since(t0)
		}(i)
	}
	<-done
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[len(latencies)/2]
	p99 := latencies[len(latencies)*99/100]
	tps := float64(totalCalls) / elapsed.Seconds()

	t.Logf("load test: %d calls in %s → %.0f tps", totalCalls, elapsed, tps)
	t.Logf("           p50=%s  p99=%s", p50, p99)

	// Sanity gates. Numbers are deliberately loose — we want to catch
	// catastrophic regressions (lock contention, leak, deadlock), not
	// shave microseconds. CI machines vary 5-10x; setting tight bounds
	// would just make the test flaky.
	if tps < 100 {
		t.Errorf("throughput too low: %.0f tps (want >= 100)", tps)
	}
	if p99 > 500*time.Millisecond {
		t.Errorf("p99 too high: %s (want <= 500ms)", p99)
	}
}

func initSession(t *testing.T, url string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"bench","version":"0"}}}`,
	))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.Header.Get("Mcp-Session-Id")
}

func callTool(t *testing.T, url, sid string, id int, fqn string) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, id, fqn)
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call %s: %v", fqn, err)
	}
	resp.Body.Close()
}
