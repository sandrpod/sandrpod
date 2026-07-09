package mcpbridge

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// NewHTTPHandler returns an http.Handler that serves both the standard MCP
// Streamable-HTTP endpoint and the sandrpod-specific /mcp/manifest extension.
//
// Mount points are:
//
//	POST/GET /mcp           — MCP Streamable HTTP (handled by mcp-go)
//	GET      /mcp/manifest  — sandrpod metadata (children, state, tool count)
//
// The caller is expected to mount this under "/" of a dedicated subtree:
//
//	mux.Handle("/mcp", bridgeHandler)
//	mux.Handle("/mcp/", bridgeHandler)
func NewHTTPHandler(mgr *ChildManager) http.Handler {
	agg := NewAggregatorServer(mgr)
	streamable := server.NewStreamableHTTPServer(agg)

	mux := http.NewServeMux()
	mux.Handle("/mcp", streamable)
	mux.Handle("/mcp/manifest", manifestHandler(mgr, false))
	// Anything else under /mcp/ (e.g. future /mcp/sse) falls through to the
	// Streamable server so it can negotiate SSE itself.
	mux.Handle("/mcp/", streamable)
	return mux
}

// NewAdminHandler exposes management operations: reload the config, restart
// a single server, disable one in memory. Intended to be mounted on a
// LOCAL-ONLY transport (Unix socket / loopback) — the operations are
// privileged and have no auth of their own.
//
// Endpoints:
//
//	GET  /admin/manifest                       — same payload as /mcp/manifest
//	POST /admin/reload                         — re-read mcp.json and apply diff
//	POST /admin/servers/{name}/restart         — stop+start one server
//	POST /admin/servers/{name}/disable         — stop one server (in-memory)
func NewAdminHandler(mgr *ChildManager) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/admin/manifest", manifestHandler(mgr, true))
	mux.HandleFunc("/admin/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := mgr.Reload(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/admin/servers/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// /admin/servers/{name}/{action}
		rest := strings.TrimPrefix(r.URL.Path, "/admin/servers/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" {
			http.Error(w, "expected /admin/servers/{name}/{action}", http.StatusBadRequest)
			return
		}
		name, action := parts[0], parts[1]
		var err error
		switch action {
		case "restart":
			err = mgr.RestartServer(r.Context(), name)
		case "disable":
			err = mgr.DisableServer(r.Context(), name)
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "action": action, "server": name})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Manifest is the JSON payload returned by /mcp/manifest.
type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	LoadedAt      time.Time       `json:"loaded_at"`
	Servers       []ChildSnapshot `json:"servers"`
	TotalTools    int             `json:"total_tools"`
	// ConfigPath is the mcp.json this bridge reads, so a caller (e.g. the CLI)
	// knows which file to read/modify to add/remove servers.
	ConfigPath string `json:"config_path,omitempty"`
}

// manifestHandler serves the bridge manifest. includeAuthURL distinguishes the
// two mounts: the public /mcp/manifest redacts pending OAuth authorization
// URLs (the browser handoff is a local, user-session concern — see the
// AuthURL field comment), while the local-only /admin/manifest keeps them so
// the desktop/tray can open the consent page.
func manifestHandler(mgr *ChildManager, includeAuthURL bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap := mgr.Snapshot()
		total := 0
		for i, s := range snap {
			if s.State == string(StateReady) {
				total += s.ToolCount
			}
			if !includeAuthURL {
				snap[i].AuthURL = ""
			}
		}
		m := Manifest{
			SchemaVersion: 1,
			LoadedAt:      time.Now().UTC(),
			Servers:       snap,
			TotalTools:    total,
			ConfigPath:    mgr.ConfigPath(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	}
}
