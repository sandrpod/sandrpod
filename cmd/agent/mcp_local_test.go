//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
)

// The local socket exists so a host on this machine skips a WAN round trip.
// Its whole safety argument is that it shares the manager with the tunnel
// entry point — same alias namespace, same permission gate, same audit. If
// it were ever wired to a separate handler the failure would be silent: the
// resourceUri a host holds (enumerated through the tunnel) would simply not
// resolve here, and a card would not appear. These tests pin that.

const localFixtureEnv = "SANDRPOD_TEST_AGENT_MCP_FIXTURE"

// TestMain doubles as a stdio MCP server when the env var is set, so the
// manager under test has a real child without a build step at test time.
func TestMain(m *testing.M) {
	if os.Getenv(localFixtureEnv) != "" {
		runLocalFixture()
		return
	}
	os.Exit(m.Run())
}

func runLocalFixture() {
	s := server.NewMCPServer("fixture", "1.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(false, true))
	s.AddTool(
		mcp.Tool{
			Name: "startable_flows", Description: "d",
			InputSchema: mcp.ToolInputSchema{Type: "object"},
			Meta: &mcp.Meta{AdditionalFields: map[string]any{
				"ui": map[string]any{"resourceUri": "ui://form"}}},
		},
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	s.AddResource(
		mcp.Resource{URI: "ui://form", Name: "form", MIMEType: "text/html;profile=mcp-app"},
		func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{
				URI: "ui://form", MIMEType: "text/html;profile=mcp-app", Text: "<h1>card</h1>",
			}}, nil
		})
	_ = server.ServeStdio(s)
}

// fixtureConfigPath writes an mcp.json whose entry re-execs this test binary.
func fixtureConfigPath(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "mcp.json")
	body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"dingliu": map[string]any{
			"command": self,
			"env":     map[string]string{localFixtureEnv: "1"},
		},
	}})
	if err := os.WriteFile(cfg, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startBridge brings up a manager plus the local socket, with HOME pointed at
// a temp dir so the socket lands somewhere disposable.
func startBridge(t *testing.T) (*mcpbridge.ChildManager, string) {
	t.Helper()
	// homedir.DataDir() derives from HOME; sun_path is 104 bytes on macOS, so
	// keep the base short rather than using t.TempDir().
	home, err := os.MkdirTemp("/tmp", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	mgr := mcpbridge.NewManager(mcpbridge.ManagerOptions{ConfigPath: fixtureConfigPath(t)})
	ctx, cancel := context.WithCancel(context.Background())
	if err := mgr.Start(ctx); err != nil {
		cancel()
		t.Fatalf("manager start: %v", err)
	}
	startMCPLocalServer(ctx, mgr)
	t.Cleanup(func() {
		cancel()
		_ = mgr.Stop(context.Background())
	})

	sock := defaultMCPLocalSocketPath()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return mgr, sock
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("socket %s never appeared", sock)
	return nil, ""
}

func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}, Timeout: 20 * time.Second}
}

func rpc(t *testing.T, c *http.Client, sid, method string, params any) (string, http.Header) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", "http://localhost/mcp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d: %s", method, resp.StatusCode, raw)
	}
	txt := string(raw)
	if i := strings.Index(txt, "data: "); i >= 0 { // Streamable-HTTP may wrap in SSE
		txt = strings.TrimSpace(txt[i+6:])
	}
	return txt, resp.Header
}

func TestMCPLocalSocket_ServesTheProtocol(t *testing.T) {
	_, sock := startBridge(t)
	c := unixClient(sock)

	out, hdr := rpc(t, c, "", "initialize", map[string]any{
		"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "probe", "version": "1"},
	})
	sid := hdr.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("no Mcp-Session-Id on the initialize response: %s", out)
	}
	if !strings.Contains(out, `"protocolVersion"`) {
		t.Errorf("initialize did not return a result: %s", out)
	}
}

// The namespace on the local socket must equal the namespace the host got
// through the tunnel, or a resourceUri it already holds will not resolve.
func TestMCPLocalSocket_SharesNamespaceWithTunnel(t *testing.T) {
	mgr, sock := startBridge(t)
	c := unixClient(sock)
	_, hdr := rpc(t, c, "", "initialize", map[string]any{
		"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "probe", "version": "1"},
	})
	sid := hdr.Get("Mcp-Session-Id")

	// What the tunnel path would advertise, straight off the manager.
	wantTool := mgr.AggregatedTools()[0].Name
	wantURI := mgr.AggregatedResources()[0].URI
	if !strings.HasPrefix(wantTool, "dingliu__") {
		t.Fatalf("fixture tool is not alias-prefixed: %q", wantTool)
	}
	if !strings.HasPrefix(wantURI, "ui://dingliu/") {
		t.Fatalf("fixture resource is not alias-namespaced: %q", wantURI)
	}

	tools, _ := rpc(t, c, sid, "tools/list", map[string]any{})
	if !strings.Contains(tools, wantTool) {
		t.Errorf("local socket does not expose %s — namespace diverged: %s", wantTool, tools)
	}

	res, _ := rpc(t, c, sid, "resources/list", map[string]any{})
	if !strings.Contains(res, wantURI) {
		t.Errorf("local socket does not expose %s — namespace diverged: %s", wantURI, res)
	}

	// The URI a host holds must be readable here, which is the actual thing
	// an MCP Apps card depends on.
	read, _ := rpc(t, c, sid, "resources/read", map[string]any{"uri": wantURI})
	// Decode rather than substring-match: encoding/json escapes < and > by
	// default, so the raw body carries \u003ch1\u003e.
	var got struct {
		Result struct {
			Contents []struct {
				URI, MIMEType, Text string
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(read), &got); err != nil {
		t.Fatalf("decode read response: %v (%s)", err, read)
	}
	if len(got.Result.Contents) == 0 {
		t.Fatalf("resources/read returned nothing: %s", read)
	}
	if got.Result.Contents[0].Text != "<h1>card</h1>" {
		t.Errorf("interface body = %q", got.Result.Contents[0].Text)
	}
	if got.Result.Contents[0].MIMEType != "text/html;profile=mcp-app" {
		t.Errorf("mimeType = %q", got.Result.Contents[0].MIMEType)
	}
}

// The socket file permissions are the entire auth boundary on POSIX — there
// is deliberately no token — so a regression to 0666 is a real hole.
func TestMCPLocalSocket_IsOwnerOnly(t *testing.T) {
	_, sock := startBridge(t)
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket (mode %v)", sock, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %#o, want 0600 — this is the only auth boundary", perm)
	}
}

// A leftover socket from an agent that died must not stop the next one.
//
// The scenario is a crash, not a restart: the file is still on disk and
// nothing is listening on it. Reproduced with SetUnlinkOnClose(false), since
// Go removes the socket file on a normal Close. An earlier version of this
// test simply started a second server while the first was still running,
// which proved nothing — the probe was answered by the first one whether or
// not the second ever bound.
func TestMCPLocalSocket_ReplacesStaleSocket(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	sock := defaultMCPLocalSocketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket did not survive Close: %v", err)
	}

	mgr := mcpbridge.NewManager(mcpbridge.ManagerOptions{ConfigPath: fixtureConfigPath(t)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop(context.Background())
	startMCPLocalServer(ctx, mgr)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c := unixClient(sock)
		req, _ := http.NewRequest("POST", "http://localhost/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+
				mcp.LATEST_PROTOCOL_VERSION+`","capabilities":{},"clientInfo":{"name":"p","version":"1"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if resp, err := c.Do(req); err == nil {
			sid := resp.Header.Get("Mcp-Session-Id")
			resp.Body.Close()
			if sid != "" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("never served over the stale path — the leftover socket was not replaced")
}

// A regular file at the canonical path is somebody else's data; clobbering it
// would be worse than declining the shortcut.
func TestMCPLocalSocket_RefusesToClobberRegularFile(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	sock := defaultMCPLocalSocketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr := mcpbridge.NewManager(mcpbridge.ManagerOptions{ConfigPath: fixtureConfigPath(t)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop(context.Background())
	startMCPLocalServer(ctx, mgr)

	time.Sleep(300 * time.Millisecond)
	body, err := os.ReadFile(sock)
	if err != nil {
		t.Fatalf("the file was removed: %v", err)
	}
	if string(body) != "not a socket" {
		t.Errorf("file contents changed to %q", body)
	}
}

// Both sockets must be gone after SIGTERM.
//
// This needs a real process: the leak was main() returning in the same
// breath as cancel(), so the cleanup goroutines never ran. Nothing
// in-process reproduces that — the bug lives in the gap between cancel and
// exit, which only exists when there is an exit.
func TestMCPSockets_RemovedOnSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	home, err := os.MkdirTemp("/tmp", "spT")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)

	agent := filepath.Join(home, "agent")
	if out, err := exec.Command("go", "build", "-o", agent, "./").CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, "mcp.json")
	body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"d": map[string]any{"command": self, "env": map[string]string{localFixtureEnv: "1"}},
	}})
	if err := os.WriteFile(cfg, body, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(agent, "-mcp-only", "-mcp-enabled", "-mcp-config", cfg,
		"-mcp-listen", "127.0.0.1:0", "-mcp-oauth-callback", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	socks := []string{
		filepath.Join(home, ".sandrpod", "mcp.sock"),
		filepath.Join(home, ".sandrpod", "mcp-local.sock"),
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		for _, s := range socks {
			if _, err := os.Stat(s); err == nil {
				n++
			}
		}
		if n == len(socks) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, s := range socks {
		if _, err := os.Stat(s); err != nil {
			t.Fatalf("%s never appeared: %v", filepath.Base(s), err)
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not exit within 20s of SIGTERM")
	}

	for _, s := range socks {
		if _, err := os.Stat(s); err == nil {
			t.Errorf("%s survived SIGTERM", filepath.Base(s))
		}
	}
}
