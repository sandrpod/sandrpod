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

// shutdownMCPBridge gracefully drains in-flight tool calls then tears
// down the manager. No-op if the bridge was never started. Called from
// main() and runMCPOnly() so SIGTERM doesn't strand clients mid-call.
func shutdownMCPBridge(drainTimeout time.Duration) {
	if currentMgr == nil {
		return
	}
	log.Printf("MCP bridge: draining (timeout=%s)...", drainTimeout)
	if err := currentMgr.Shutdown(context.Background(), drainTimeout); err != nil {
		log.Printf("MCP bridge: drain incomplete: %v", err)
	} else {
		log.Printf("MCP bridge: drain complete")
	}
}

// installMCPBridge starts the optional MCP transport bridge and returns
// its http.Handler. The handler is returned even when mcp.json doesn't
// exist yet — the bridge comes up with zero children, watches the
// parent dir, and picks up a later file create automatically. This
// matches how users actually install things: run sandrpod-agent first,
// figure out where the config goes second.
//
// Returns nil only when --mcp-enabled is explicitly false (operator
// opted out).
func installMCPBridge(ctx context.Context) http.Handler {
	if !*mcpEnabled {
		return nil
	}

	path := *mcpConfigPath
	if path == "" {
		path = mcpbridge.DefaultConfigPath()
	}

	perm, permDesc := buildMCPPermissionGate()
	auditSink, auditDesc := buildMCPAuditSink()

	mgr := mcpbridge.NewManager(mcpbridge.ManagerOptions{
		ConfigPath: path,
		HotReload:  *mcpHotReload,
		Permission: perm,
		Audit:      auditSink,
		Logger:     log.Default(),
		OAuth:      buildMCPOAuthOptions(),
	})
	log.Printf("MCP bridge: permission=%s audit=%s", permDesc, auditDesc)
	if err := mgr.Start(ctx); err != nil {
		log.Printf("MCP bridge: start failed (%s): %v — bridge disabled", path, err)
		return nil
	}

	snap := mgr.Snapshot()
	if len(snap) == 0 {
		log.Printf("MCP bridge ready: config=%s no servers loaded yet (hot_reload=%v will pick up a later mcp.json create)",
			path, *mcpHotReload)
	} else {
		log.Printf("MCP bridge ready: config=%s servers=%d hot_reload=%v", path, len(snap), *mcpHotReload)
		for _, s := range snap {
			log.Printf("  %-20s state=%-10s tools=%d", s.Name, s.State, s.ToolCount)
		}
	}

	currentMgr = mgr
	startMCPAdminServer(ctx, mgr)

	// Wrap with shared-secret middleware. When --mcp-token is unset we
	// fall through unchanged (backward compatible). When set, every
	// /mcp request must carry the matching Bearer; admin endpoints stay
	// on the unix socket and use file-permission auth instead.
	publicHandler := mcpbridge.NewHTTPHandler(mgr)
	if *mcpToken != "" {
		publicHandler = mcpbridge.TokenMiddleware(*mcpToken, *mcpGuardManifest, publicHandler)
		log.Printf("MCP bridge: shared-secret auth enabled (token length=%d, guard_manifest=%v)", len(*mcpToken), *mcpGuardManifest)
	} else {
		log.Printf("MCP bridge: WARNING — no --mcp-token set; any caller that reaches /mcp can invoke tools")
	}
	return publicHandler
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

	// Construction never fails: a corrupt grants file degrades to empty
	// grants (prompt for everything) instead of the old error path, which
	// returned a nil gate — silently replaced by allow-all in the bridge.
	scope := parseGrantScope(*mcpGrantScope)
	adapter := newMCPPermissionAdapter(notifier, defaultMCPGrantsPath(), scope)
	return adapter, "mode=" + mode + " scope=" + string(scope) + " grants=" + defaultMCPGrantsPath()
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

	// Stop accepting new HTTP connections first so the drain window
	// isn't extended by clients that arrive after we've decided to
	// shut down. http.Server.Shutdown drains in-flight HTTP requests
	// up to its own context — those requests are what's calling into
	// the bridge, so we then ask the bridge to drain whatever they
	// kicked off into child stdio.
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer c2()
	_ = srv.Shutdown(shutdownCtx)
	shutdownMCPBridge(10 * time.Second)
}
