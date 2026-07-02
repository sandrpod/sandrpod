// MCP transport bridge integration for the tray.
//
// The agent's mcpbridge admin endpoints live on a Unix socket
// (~/.sandrpod/mcp.sock). This file gives the tray:
//
//  1. A thin client that dials that socket.
//  2. Settings-page endpoints under /api/mcp/* that proxy through.
//  3. A "MCP Servers" submenu showing per-server state with click-to-restart.
//
// When the agent isn't running (no socket) we surface that as "未连接" instead
// of crashing the menu — the tray and the agent have independent lifetimes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

// mcpAdminSocketPath mirrors cmd/agent/mcp_admin.go. Kept in sync by
// convention rather than shared constant to avoid creating a circular
// import (cmd/agent <-> cmd/sandrpod-tray would be ugly).
func mcpAdminSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "mcp.sock"
	}
	return filepath.Join(home, ".sandrpod", "mcp.sock")
}

// mcpAdminClient is a minimal HTTP-over-unix-socket client.
type mcpAdminClient struct {
	sockPath string
	hc       *http.Client
}

func newMCPAdminClient() *mcpAdminClient {
	sock := mcpAdminSocketPath()
	return &mcpAdminClient{
		sockPath: sock,
		hc: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

func (c *mcpAdminClient) get(path string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, "http://unix"+path, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("admin GET %s: %d %s", path, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *mcpAdminClient) post(path string, body []byte) error {
	req, _ := http.NewRequest(http.MethodPost, "http://unix"+path, bytes.NewReader(body))
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("admin POST %s: %d %s", path, resp.StatusCode, string(b))
	}
	return nil
}

// mcpServerInfo mirrors mcpbridge.ChildSnapshot. Duplicated to avoid an
// import cycle and to keep the tray buildable when the bridge package
// evolves independently.
type mcpServerInfo struct {
	Name      string `json:"name"`
	Alias     string `json:"alias"`
	State     string `json:"state"`
	Command   string `json:"command"`
	ToolCount int    `json:"tool_count"`
	Restarts  int    `json:"restart_count"`
	LastError string `json:"last_error,omitempty"`
}

type mcpManifest struct {
	Servers    []mcpServerInfo `json:"servers"`
	TotalTools int             `json:"total_tools"`
}

// ---- HTTP page handlers (mounted from http.go's startHTTP) ----

func httpMCPManifest(w http.ResponseWriter, r *http.Request) {
	cli := newMCPAdminClient()
	var m mcpManifest
	if err := cli.get("/admin/manifest", &m); err != nil {
		http.Error(w, "agent not reachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

func httpMCPReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := newMCPAdminClient().post("/admin/reload", nil); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func httpMCPServerAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name   string `json:"name"`
		Action string `json:"action"` // restart | disable
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.Action == "" {
		http.Error(w, "name and action required", http.StatusBadRequest)
		return
	}
	if err := newMCPAdminClient().post("/admin/servers/"+body.Name+"/"+body.Action, nil); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Tray submenu wiring ----

// mcpMenu holds the dynamic submenu items so the refresh goroutine can
// update their titles in place.
type mcpMenu struct {
	mu     sync.Mutex
	parent *systray.MenuItem
	items  map[string]*mcpMenuItem // server name -> item handle
	reload *systray.MenuItem
}

type mcpMenuItem struct {
	mi   *systray.MenuItem
	stop chan struct{}
}

// initMCPMenu builds the "MCP 服务" submenu under the main tray menu and
// starts a refresh goroutine that polls the agent every 10s.
func initMCPMenu() *mcpMenu {
	parent := systray.AddMenuItem("MCP 服务", "聚合本机 stdio MCP 服务器")
	reload := parent.AddSubMenuItem("重载 mcp.json", "重新读取配置并应用变更")
	parent.AddSubMenuItemCheckbox("（未连接）", "", false).Disable()

	m := &mcpMenu{
		parent: parent,
		items:  map[string]*mcpMenuItem{},
		reload: reload,
	}

	go func() {
		for range time.Tick(10 * time.Second) {
			m.refresh()
		}
	}()

	go func() {
		for range reload.ClickedCh {
			if err := newMCPAdminClient().post("/admin/reload", nil); err != nil {
				// Surface in tooltip — the user has no other feedback channel
				reload.SetTooltip("reload failed: " + err.Error())
				continue
			}
			reload.SetTooltip("reloaded ok at " + time.Now().Format(time.Kitchen))
			m.refresh()
		}
	}()

	m.refresh()
	return m
}

func (m *mcpMenu) refresh() {
	cli := newMCPAdminClient()
	var mfst mcpManifest
	if err := cli.get("/admin/manifest", &mfst); err != nil {
		m.parent.SetTitle("MCP 服务（未连接）")
		return
	}
	m.parent.SetTitle(fmt.Sprintf("MCP 服务（%d server, %d tools）", len(mfst.Servers), mfst.TotalTools))

	m.mu.Lock()
	defer m.mu.Unlock()

	seen := map[string]bool{}
	sort.Slice(mfst.Servers, func(i, j int) bool { return mfst.Servers[i].Name < mfst.Servers[j].Name })
	for _, s := range mfst.Servers {
		seen[s.Name] = true
		title := mcpServerMenuTitle(s)
		if item, ok := m.items[s.Name]; ok {
			item.mi.SetTitle(title)
			item.mi.SetTooltip(mcpServerTooltip(s))
			continue
		}
		mi := m.parent.AddSubMenuItem(title, mcpServerTooltip(s))
		stop := make(chan struct{})
		m.items[s.Name] = &mcpMenuItem{mi: mi, stop: stop}
		go runServerItemClicks(mi, stop, s.Name)
	}
	// Hide items for servers that disappeared. systray has no "remove
	// item" API so we just relabel + disable.
	for name, item := range m.items {
		if !seen[name] {
			item.mi.SetTitle("(removed) " + name)
			item.mi.Disable()
		}
	}
}

func mcpServerMenuTitle(s mcpServerInfo) string {
	mark := "●"
	switch s.State {
	case "ready":
		mark = "✓"
	case "failed":
		mark = "⚠"
	case "starting", "restarting":
		mark = "…"
	case "stopped":
		mark = "○"
	}
	return fmt.Sprintf("%s %s  (%s, %d tools)", mark, s.Name, s.State, s.ToolCount)
}

func mcpServerTooltip(s mcpServerInfo) string {
	if s.LastError != "" {
		return s.LastError
	}
	return fmt.Sprintf("alias=%s restarts=%d", s.Alias, s.Restarts)
}

// runServerItemClicks listens for clicks on a server's submenu item and
// fires a restart. We don't add a "disable" menu entry per server in MVP
// — restart is the 90% use case.
func runServerItemClicks(mi *systray.MenuItem, stop <-chan struct{}, name string) {
	for {
		select {
		case <-stop:
			return
		case <-mi.ClickedCh:
			if err := newMCPAdminClient().post("/admin/servers/"+name+"/restart", nil); err != nil {
				mi.SetTooltip("restart failed: " + err.Error())
				continue
			}
			mi.SetTooltip("restart triggered at " + time.Now().Format(time.Kitchen))
		}
	}
}
