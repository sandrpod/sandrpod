package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sandrpod/sandrpod/pkg/audit"
	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
	"github.com/sandrpod/sandrpod/pkg/notify"
	"github.com/sandrpod/sandrpod/pkg/permission"
)

// currentMgr is set by installMCPBridge for callers (admin socket) that
// need the manager handle. The bridge is a singleton per-agent so a
// package-level var is OK; protected by the fact that installMCPBridge is
// only called once from main().
var currentMgr *mcpbridge.ChildManager

// installMCPBridge starts the optional MCP transport bridge and returns its
// http.Handler. Returns nil when:
//   - --mcp-enabled is false; or
//   - the mcp.json path resolves to a missing file (treated as "not
//     configured", not an error — we don't want every employee PC without
//     MCP to fail).
//
// A non-nil handler means the bridge is up; even if individual children
// failed to spawn, the handler serves /mcp/manifest so operators can see why.
func installMCPBridge(ctx context.Context) http.Handler {
	if !*mcpEnabled {
		return nil
	}

	path := *mcpConfigPath
	if path == "" {
		path = mcpbridge.DefaultConfigPath()
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			log.Printf("MCP bridge: no config at %s, bridge disabled", path)
			return nil
		}
		log.Printf("MCP bridge: stat %s failed: %v", path, err)
		return nil
	}

	perm, permDesc := buildMCPPermissionGate()
	auditSink, auditDesc := buildMCPAuditSink()

	mgr := mcpbridge.NewManager(mcpbridge.ManagerOptions{
		ConfigPath: path,
		HotReload:  *mcpHotReload,
		Permission: perm,
		Audit:      auditSink,
		Logger:     log.Default(),
	})
	log.Printf("MCP bridge: permission=%s audit=%s", permDesc, auditDesc)
	if err := mgr.Start(ctx); err != nil {
		log.Printf("MCP bridge: start failed (%s): %v — bridge disabled", path, err)
		return nil
	}

	snap := mgr.Snapshot()
	log.Printf("MCP bridge ready: config=%s servers=%d hot_reload=%v", path, len(snap), *mcpHotReload)
	for _, s := range snap {
		log.Printf("  %-20s state=%-10s tools=%d", s.Name, s.State, s.ToolCount)
	}

	currentMgr = mgr
	startMCPAdminServer(ctx, mgr)
	return mcpbridge.NewHTTPHandler(mgr)
}

// buildMCPPermissionGate constructs a PermissionGate for the bridge. When
// the permission mode is "off" we return nil (the manager substitutes its
// own allow-all gate). Otherwise we reuse the same tray IPC notifier the
// path-permission code uses, with the in-process notify.Prompter as
// fallback. Grants are persisted in ~/.sandrpod/mcp_grants.json.
func buildMCPPermissionGate() (mcpbridge.PermissionGate, string) {
	mode := strings.ToLower(strings.TrimSpace(*permissionMode))
	if mode == "" || mode == "off" {
		return nil, "off (allow-all)"
	}

	var notifier permission.Notifier
	switch mode {
	case "prompt":
		sock, err := permission.DefaultSocketPath()
		if err != nil {
			log.Printf("MCP bridge: locate authz socket failed: %v — falling back to in-process prompter", err)
			notifier = notify.NewPrompter()
		} else {
			if override := os.Getenv("SANDRPOD_AUTHZ_SOCKET"); override != "" {
				sock = override
			}
			ipc := permission.NewIPCClient(sock)
			ipc.FallbackOnUnavailable = notify.NewPrompter()
			notifier = ipc
		}
	case "strict":
		notifier = permission.NopNotifier{}
	default:
		log.Printf("MCP bridge: unknown permission-mode %q — using NopNotifier (fail-close)", mode)
		notifier = permission.NopNotifier{}
	}

	adapter, err := newMCPPermissionAdapter(notifier, defaultMCPGrantsPath())
	if err != nil {
		log.Printf("MCP bridge: build permission adapter failed: %v — fail-close", err)
		return nil, "broken (deny-all on error)"
	}
	return adapter, "mode=" + mode + " grants=" + defaultMCPGrantsPath()
}

// buildMCPAuditSink reuses the shared audit recorder set up by
// installAuditPipeline. Reusing the same *Recorder is critical: each
// Recorder owns its own mutex and rotation state, so two concurrent
// recorders writing the same active.log race on rotation and can lose
// events at the boundary.
//
// In --mcp-only mode the path-permission pipeline isn't initialised, so
// the shared recorder is nil; we fall back to creating a dedicated one
// (still safe because no other writer exists in that mode).
func buildMCPAuditSink() (mcpbridge.AuditSink, string) {
	if sharedAuditRecorder != nil {
		return &mcpAuditAdapter{rec: sharedAuditRecorder}, "shared with path-permission pipeline"
	}
	dir := *auditDir
	if dir == "" {
		def, err := audit.DefaultDir()
		if err != nil {
			log.Printf("MCP bridge: locate audit dir failed: %v — audit disabled", err)
			return nil, "disabled (no dir)"
		}
		dir = def
	}
	rec, err := audit.NewRecorder(audit.Options{Dir: dir})
	if err != nil {
		log.Printf("MCP bridge: audit recorder failed: %v — audit disabled", err)
		return nil, "disabled (open failed)"
	}
	return &mcpAuditAdapter{rec: rec}, "dedicated " + dir
}

// runMCPOnly is the --mcp-only entry point: bridge only, no tunnel, no
// toolbox. Useful for local-LAN MCP aggregation and developer testing.
// Blocks until the process receives SIGINT/SIGTERM.
func runMCPOnly() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := installMCPBridge(ctx)
	if handler == nil {
		log.Fatalf("--mcp-only: bridge could not start (check -mcp-config and earlier logs)")
	}

	srv := &http.Server{
		Addr:              *mcpListen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("MCP-only mode: listening on http://%s/mcp (manifest: http://%s/mcp/manifest)", *mcpListen, *mcpListen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("MCP-only serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("MCP-only: shutting down")
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	_ = srv.Shutdown(shutdownCtx)
}
