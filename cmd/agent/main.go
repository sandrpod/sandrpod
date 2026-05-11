// Copyright 2024 SandrPod
// sandrpod-agent — local machine sandbox agent
//
// Registers the local machine directly as a SandrPod Sandbox without
// requiring a Poder/Docker layer. Connects to the API Server via a
// WebSocket + yamux reverse tunnel and embeds Toolbox to provide
// remote AI agents with code execution capabilities.
//
// Usage:
//
//	sandrpod-agent -api-url=https://your-api-server.com -name=my-laptop -token=<api-key>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/sandrpod/sandrpod/pkg/audit"
	"github.com/sandrpod/sandrpod/pkg/notify"
	"github.com/sandrpod/sandrpod/pkg/permission"
	"github.com/sandrpod/sandrpod/pkg/toolbox"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// agentVersion is overridden at build time via -ldflags "-X main.agentVersion=…".
// Default "dev" makes local builds clearly distinguishable on the server side.
var agentVersion = "dev"

// auditAdapter bridges pkg/audit.Recorder to permission.AuditSink, keeping
// pkg/permission free of any pkg/audit dependency.
type auditAdapter struct {
	rec *audit.Recorder
}

func (a *auditAdapter) Record(source, decision, path, mode, caller, sessionID, reason, matchedCommand string) {
	if a == nil || a.rec == nil {
		return
	}
	_ = a.rec.Record(audit.Event{
		Source:         audit.Source(source),
		Decision:       decision,
		Path:           path,
		Mode:           mode,
		Caller:         caller,
		SessionID:      sessionID,
		Reason:         reason,
		MatchedCommand: matchedCommand,
	})
}

func envOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

var (
	apiURL    = flag.String("api-url", envOr("SANDRPOD_API_URL", "http://localhost:8080"), "API Server URL")
	name      = flag.String("name", envOr("SANDRPOD_SANDBOX_NAME", ""), "Sandbox name (required, must be unique per API server)")
	token     = flag.String("token", envOr("SANDRPOD_TOKEN", ""), "API token for authentication")
	workDir   = flag.String("work-dir", envOr("SANDRPOD_WORK_DIR", ""), "Working directory for code execution (default: current dir)")
	reconnect = flag.Duration("reconnect", 5*time.Second, "Reconnect delay on disconnect")
	help      = flag.Bool("help", false, "Show help")

	// Permission gate — opt-in for employee PC mode where the OS user
	// expects desktop consent prompts before AI touches anything outside
	// the sandbox work_dir.
	//
	// Modes:
	//   off     — legacy behavior; only the system-path blacklist applies
	//   prompt  — work_dir is silent-allow, anything else asks the human
	//             via the platform notifier (osascript on macOS, …)
	//   strict  — like prompt, but no notifier is wired (NopNotifier);
	//             everything outside work_dir is silently denied. Useful
	//             for headless setups that intentionally refuse to run
	//             outside the sandbox.
	permissionMode = flag.String("permission-mode", envOr("SANDRPOD_PERMISSION_MODE", "off"), "Permission gate: off | prompt | strict")
	permissionFile = flag.String("permission-file", envOr("SANDRPOD_PERMISSION_FILE", ""), "Override path to permissions.json (default: ~/.sandrpod/permissions.json)")

	// Audit upload — independent of the permission gate so an operator
	// running --permission-mode=off can still get pure observability
	// (every file access logged, nothing blocked). Off by default.
	auditDir         = flag.String("audit-dir", envOr("SANDRPOD_AUDIT_DIR", ""), "Audit log directory (default: ~/.sandrpod/audit). Empty disables local audit.")
	auditUploadURL   = flag.String("audit-upload-url", envOr("SANDRPOD_AUDIT_UPLOAD_URL", ""), "Endpoint to POST audit batches to. Empty disables upload (still logs locally).")
	auditUploadToken = flag.String("audit-upload-token", envOr("SANDRPOD_AUDIT_UPLOAD_TOKEN", ""), "Bearer token sent with audit upload requests. Defaults to -token if empty.")
)

func main() {
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	if *name == "" {
		fmt.Fprintln(os.Stderr, "Error: -name is required")
		flag.Usage()
		os.Exit(1)
	}

	if *workDir != "" {
		if err := os.Chdir(*workDir); err != nil {
			log.Fatalf("Failed to change to work dir %s: %v", *workDir, err)
		}
	}

	log.Printf("Starting SandrPod Agent")
	log.Printf("Sandbox name: %s", *name)
	log.Printf("API Server:   %s", *apiURL)

	ctx, cancel := context.WithCancel(context.Background())

	// Build the handler once and reuse across reconnects.
	// The toolbox listens only on the yamux tunnel (not a public port),
	// so no separate auth token is needed here — the API Server token
	// already controls tunnel access.
	tb := toolbox.NewServer("", "")

	// If permission gating is requested, install a manager BEFORE accepting
	// any traffic so there is no window where unsanctioned access can slip
	// through during startup.
	if err := installPermissionGate(tb); err != nil {
		log.Fatalf("permission gate setup failed: %v", err)
	}

	// Audit pipeline (recorder + optional uploader). Wired AFTER the
	// permission gate so we can hand the manager the audit sink.
	if err := installAuditPipeline(ctx, tb); err != nil {
		log.Fatalf("audit pipeline setup failed: %v", err)
	}

	tbHandler := tb.Handler()
	agentHandler := newAgentMux(tbHandler, *name)

	go connectLoop(ctx, agentHandler)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down...")
	cancel()
}

// installPermissionGate wires the permission manager into the toolbox's
// embedded executor based on the --permission-mode flag.
//
//   - off:    no-op (legacy behavior).
//   - prompt: load permissions.json, install the platform notifier
//             (osascript modal on macOS).
//   - strict: load permissions.json, install NopNotifier so every path
//             outside work_dir is silently denied.
//
// Any failure (unreadable permissions file, unsupported platform when in
// prompt mode) is fatal — we refuse to start with a broken gate, since
// fail-open is worse than not running.
func installPermissionGate(tb *toolbox.Server) error {
	mode := strings.ToLower(strings.TrimSpace(*permissionMode))
	if mode == "" || mode == "off" {
		return nil
	}

	storePath := *permissionFile
	if storePath == "" {
		def, err := permission.DefaultStorePath()
		if err != nil {
			return fmt.Errorf("locate permissions.json: %w", err)
		}
		storePath = def
	}
	store, err := permission.LoadStore(storePath)
	if err != nil {
		return fmt.Errorf("load permissions.json (%s): %w", storePath, err)
	}

	var notifier permission.Notifier
	switch mode {
	case "prompt":
		// Two-tier prompter:
		//
		//   primary  = sandrpod-tray over Unix socket (the user-session GUI)
		//   fallback = in-process MacPrompter (if tray isn't running yet)
		//
		// In production the agent should always be paired with a tray
		// (installed as a LaunchAgent for automatic startup). The fallback
		// is for first-run / dev / "tray crashed and is being restarted"
		// scenarios so a missing tray doesn't fail-close every request and
		// confuse the operator into thinking permission gating is broken.
		//
		// On Linux/Windows the in-process fallback is itself a stub that
		// returns deny-with-error; that's acceptable because we expect the
		// tray to be present on every employee PC, and "unsupported
		// platform" is exactly the error operators need to see.
		sock, err := permission.DefaultSocketPath()
		if err != nil {
			return fmt.Errorf("locate authz socket: %w", err)
		}
		ipcOverride := os.Getenv("SANDRPOD_AUTHZ_SOCKET")
		if ipcOverride != "" {
			sock = ipcOverride
		}
		ipc := permission.NewIPCClient(sock)
		// Fallback to in-process prompter if the tray daemon isn't reachable.
		// notify.NewPrompter() is build-tag-selected: osascript on macOS,
		// zenity/kdialog on Linux, PowerShell MessageBox on Windows.
		ipc.FallbackOnUnavailable = notify.NewPrompter()
		notifier = ipc
	case "strict":
		notifier = permission.NopNotifier{}
	default:
		return fmt.Errorf("unknown --permission-mode %q (expected off|prompt|strict)", mode)
	}

	exec := tb.Executor()
	mgr, err := permission.NewManager(permission.Options{
		Store:    store,
		Notifier: notifier,
		WorkDir:  exec.GetWorkDirForPermission(),
	})
	if err != nil {
		return fmt.Errorf("build permission manager: %w", err)
	}
	exec.SetPermissionManager(mgr)

	log.Printf("Permission gate enabled (mode=%s, store=%s, work_dir=%s)",
		mode, storePath, exec.GetWorkDirForPermission())
	return nil
}

// installAuditPipeline wires the local NDJSON recorder and (if configured)
// the background uploader. The recorder is wired into the permission
// manager only when permission gating is on; we don't want to silently log
// "every file access was allowed" lines when there is no policy at all.
func installAuditPipeline(ctx context.Context, tb *toolbox.Server) error {
	dir := *auditDir
	if dir == "" {
		def, err := audit.DefaultDir()
		if err != nil {
			return fmt.Errorf("locate audit dir: %w", err)
		}
		dir = def
	}
	rec, err := audit.NewRecorder(audit.Options{Dir: dir})
	if err != nil {
		return fmt.Errorf("audit recorder: %w", err)
	}
	log.Printf("audit recorder writing to %s", dir)

	// Hand the recorder to the permission manager (if any).
	if mgr := tb.Executor().PermissionManager(); mgr != nil {
		mgr.SetAuditSink(&auditAdapter{rec: rec})
	}

	// Upload is optional. If URL is empty we still log locally — useful
	// for forensic-only deployments where ops will pull the files manually.
	if *auditUploadURL == "" {
		log.Printf("audit upload disabled (no --audit-upload-url); events will accumulate in %s", dir)
		return nil
	}
	tok := *auditUploadToken
	if tok == "" {
		tok = *token
	}
	uploader, err := audit.NewUploader(audit.UploaderOptions{
		URL:          *auditUploadURL,
		Token:        tok,
		Recorder:     rec,
		AgentVersion: agentVersion,
		SandboxName:  *name,
		HostOS:       runtime.GOOS,
		HostArch:     runtime.GOARCH,
	})
	if err != nil {
		return fmt.Errorf("audit uploader: %w", err)
	}
	if uploader != nil {
		uploader.Start(ctx)
		log.Printf("audit upload enabled — POST → %s every 30s (batch size 200)", *auditUploadURL)
	}
	return nil
}

// newAgentMux wraps the Toolbox handler with poder-protocol translation.
//
// The API Server always uses poder's URL schema when proxying to workers.
// This mux translates those paths to the toolbox-native paths:
//
//	POST /execute?sandbox=<name>              → POST /process
//	*    /toolbox/<name>/<subPath>            → * /<subPath>
//	*    /process/session/<name>              → * /process/session
//	*    /process/session/<name>/<rest>       → * /process/session/<rest>
//	GET  /logs?sandbox=<name>                → empty log response
//	*    everything else (/stream, /pty/, …) → toolbox directly (same paths)
func newAgentMux(tb http.Handler, sandboxName string) http.Handler {
	mux := http.NewServeMux()

	// /execute → /process
	mux.HandleFunc("/execute", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/process"
		tb.ServeHTTP(w, r2)
	})

	// /logs → empty (agent is a local process, no container logs)
	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"logs": ""})
	})

	// /toolbox/<name>/<subPath> → /<subPath>
	toolboxPrefix := "/toolbox/" + sandboxName + "/"
	mux.HandleFunc("/toolbox/", func(w http.ResponseWriter, r *http.Request) {
		sub, found := strings.CutPrefix(r.URL.Path, toolboxPrefix)
		if !found {
			// fallback: strip /toolbox/<anything>/
			rest := strings.TrimPrefix(r.URL.Path, "/toolbox/")
			if idx := strings.Index(rest, "/"); idx >= 0 {
				sub = rest[idx+1:]
			} else {
				http.Error(w, "invalid toolbox path", http.StatusBadRequest)
				return
			}
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + sub
		tb.ServeHTTP(w, r2)
	})

	// /process/session/<name>          → /process/session
	// /process/session/<name>/<rest>   → /process/session/<rest>
	//
	// The API Server prefixes the sandbox name into session paths so that
	// Poder nodes (which serve multiple sandboxes) can route correctly.
	// The agent serves exactly one sandbox, so strip the name prefix.
	sessionWithName := "/process/session/" + sandboxName
	mux.HandleFunc("/process/session/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		var toolboxPath string
		switch {
		case path == sessionWithName || path == sessionWithName+"/":
			toolboxPath = "/process/session"
		case strings.HasPrefix(path, sessionWithName+"/"):
			// /process/session/<name>/<rest> → /process/session/<rest>
			toolboxPath = "/process/session/" + strings.TrimPrefix(path, sessionWithName+"/")
		default:
			// path doesn't carry our sandbox name — pass through unchanged
			toolboxPath = path
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = toolboxPath
		tb.ServeHTTP(w, r2)
	})

	// Everything else (/stream, /pty/, /health, /info, …) → toolbox directly.
	mux.Handle("/", tb)

	return mux
}

// connectLoop dials the API Server and serves agentHandler over the yamux tunnel.
// Reconnects automatically on disconnect.
func connectLoop(ctx context.Context, handler http.Handler) {
	wsURL := toWS(*apiURL) + "/ws/sandbox/connect"

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		headers := buildHeaders()
		log.Printf("Connecting to %s as sandbox %q...", *apiURL, *name)

		wsConn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, headers)
		if err != nil {
			statusCode := 0
			if resp != nil {
				statusCode = resp.StatusCode
			}
			log.Printf("Connect failed (HTTP %d): %v — retry in %s", statusCode, err, *reconnect)
			select {
			case <-ctx.Done():
				return
			case <-time.After(*reconnect):
			}
			continue
		}
		log.Printf("Connected — sandbox %q is online", *name)

		cfg := yamux.DefaultConfig()
		cfg.KeepAliveInterval = 30 * time.Second
		session, err := yamux.Server(tunnel.NewWSConn(wsConn), cfg)
		if err != nil {
			log.Printf("yamux.Server failed: %v", err)
			wsConn.Close()
			continue
		}

		httpSrv := &http.Server{Handler: handler}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := httpSrv.Serve(session); err != nil && err != http.ErrServerClosed {
				log.Printf("Tunnel serve ended: %v", err)
			}
		}()

		select {
		case <-ctx.Done():
			httpSrv.Close()
			return
		case <-done:
		}
		httpSrv.Close()
		log.Printf("Disconnected — retry in %s", *reconnect)
		select {
		case <-ctx.Done():
			return
		case <-time.After(*reconnect):
		}
	}
}

// buildHeaders constructs WebSocket headers for sandbox registration.
func buildHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Sandbox-Name", *name)
	h.Set("X-Sandbox-Arch", runtime.GOARCH)
	h.Set("X-Sandbox-OS", runtime.GOOS)
	h.Set("X-Sandbox-OS-Version", getOSVersion())
	if *token != "" {
		h.Set("Authorization", "Bearer "+*token)
	}
	return h
}

// toWS converts http(s):// to ws(s)://
func toWS(u string) string {
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + u[len("https://"):]
	case strings.HasPrefix(u, "http://"):
		return "ws://" + u[len("http://"):]
	}
	return u
}

// getOSVersion is provided by os_version_{unix,windows}.go.
