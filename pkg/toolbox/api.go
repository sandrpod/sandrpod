// Copyright 2024 SandrPod
// Toolbox API - code execution service

package toolbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sandrpod/sandrpod/pkg/permission"
)

// writeError writes an appropriate HTTP status code based on the error type:
// ErrAccessDenied → 403 Forbidden, all others → 500 Internal Server Error
func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrAccessDenied) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// StreamRequest is the request payload for streaming code execution (passed as query parameters).
type StreamRequest struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Timeout  int    `json:"timeout"`
}

// ProcessRequest is the request payload for a code execution job.
type ProcessRequest struct {
	Language string `json:"language"` // python, node, bash
	Code     string `json:"code"`
	Timeout  int    `json:"timeout"` // seconds
}

// ProcessResult holds the output of a code execution job.
type ProcessResult struct {
	ExitCode  int       `json:"exit_code"`
	Stdout    string    `json:"stdout"`
	Stderr    string    `json:"stderr"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// HealthStatus reports the health of the Toolbox service.
type HealthStatus struct {
	Status    string `json:"status"`
	Docker    bool   `json:"docker"`
	Timestamp int64  `json:"timestamp"`
}

// EnvironmentInfo describes the sandbox runtime environment (useful for AI-generated scripts).
type EnvironmentInfo struct {
	Arch          string `json:"arch"`           // e.g. amd64, arm64
	OS            string `json:"os"`             // e.g. linux
	OSVersion     string `json:"os_version"`     // e.g. Ubuntu 22.04.3 LTS
	KernelVersion string `json:"kernel_version"` // e.g. 5.15.0-91-generic
	Shell         string `json:"shell"`          // e.g. /bin/bash
	WorkDir       string `json:"work_dir"`       // default working directory
	// Home is the sandbox user's home directory. Consumers (e.g. a platform's
	// digital-employee personal-skill discovery) need an absolute anchor for
	// ~/.sandrpod/skills/ — work_dir is task-scoped and can't derive $HOME.
	Home string `json:"home"` // e.g. /Users/alice, C:\Users\alice
}

// Server is the Toolbox HTTP server.
type Server struct {
	addr           string
	token          string // Auth token; empty string disables authentication
	executor       *Executor
	ptyHandler     *PtyHandler
	sessionManager *SessionManager
	server         *http.Server
	mu             sync.RWMutex
	requests       int64
	startTime      time.Time
	ctx            context.Context
	cancel         context.CancelFunc

	// mcpHandler, when set, is mounted at /mcp (and /mcp/...) by Handler().
	// It lets standalone toolbox deployments expose the same MCP bridge the
	// agent does. Nil = no MCP surface (the route stays unregistered → 404).
	// Set once before Start() via SetMCPHandler.
	mcpHandler http.Handler

	// kernels backs the E2B-compatible stateful code interpreter
	// (/code-interpreter/execute). Lazily created on first use.
	kernels     *KernelManager
	kernelsOnce sync.Once

	// procs backs the E2B-compatible pid-addressed process table
	// (/procmgr/*: background commands, connect, stdin, signal, PTY).
	procs     *ProcManager
	procsOnce sync.Once

	// watchers backs the E2B-compatible directory watch surface (/watch/*).
	watchers     *WatchManager
	watchersOnce sync.Once
}

// watchManager returns the lazily-initialized filesystem watch manager.
func (s *Server) watchManager() *WatchManager {
	s.watchersOnce.Do(func() { s.watchers = NewWatchManager() })
	return s.watchers
}

// codeInterpreter returns the lazily-initialized kernel manager.
func (s *Server) codeInterpreter() *KernelManager {
	s.kernelsOnce.Do(func() { s.kernels = NewKernelManager("python3") })
	return s.kernels
}

// procManager returns the lazily-initialized managed process table.
func (s *Server) procManager() *ProcManager {
	s.procsOnce.Do(func() { s.procs = NewProcManager() })
	return s.procs
}

// codeInterpreterHandler implements POST /code-interpreter/execute:
// {"code": "...", "context_id": "..."} → stateful CodeResult. Backs the E2B
// code-interpreter run_code contract (the gateway adapts it).
func (s *Server) codeInterpreterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Code      string `json:"code"`
		ContextID string `json:"context_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ContextID == "" {
		req.ContextID = "default"
	}
	if !s.gateExec(w, req.Code) {
		return
	}
	res, err := s.codeInterpreter().Execute(req.ContextID, req.Code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// codeContextsHandler: POST creates a context, GET lists them.
func (s *Server) codeContextsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Language string `json:"language"`
			Cwd      string `json:"cwd"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ci := s.codeInterpreter().CreateContext(req.Language, req.Cwd)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ci)
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.codeInterpreter().ListContexts())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// codeContextByIDHandler: DELETE /contexts/{id} removes; POST /contexts/{id}/restart restarts.
func (s *Server) codeContextByIDHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/code-interpreter/contexts/")
	id, action, _ := strings.Cut(rest, "/")
	switch {
	case action == "restart" && r.Method == http.MethodPost:
		s.codeInterpreter().Restart(id)
		w.WriteHeader(http.StatusNoContent)
	case action == "" && r.Method == http.MethodDelete:
		s.codeInterpreter().Close(id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// SetMCPHandler installs the optional MCP bridge handler mounted at /mcp.
// Call before Start(); calling after the mux is built has no effect on the
// already-served handler.
func (s *Server) SetMCPHandler(h http.Handler) {
	s.mcpHandler = h
}

const CleanupTimeout = 5 * time.Second

// NewServer creates a Toolbox server. Authentication is disabled when token is empty.
// Executor exposes the embedded Executor so external code (notably the agent
// binary) can install policy hooks like a permission manager. The returned
// pointer is owned by the Server; callers must not retain it past the
// Server's lifetime.
func (s *Server) Executor() *Executor { return s.executor }

// gateExec runs a command/code string through the permission manager's
// CheckExec — the same deny-list scan + audit record the /process path applies.
// It is called from the alternate execution surfaces (/procmgr/start, session
// exec, /code-interpreter/execute) so they leave an audit trail and honor the
// command deny-list instead of silently bypassing both. Returns false (and
// writes a 403) when the command is denied. No-op — returns true — when no
// permission manager is installed (Docker/poder, or --permission-mode=off).
func (s *Server) gateExec(w http.ResponseWriter, command string) bool {
	mgr := s.executor.PermissionManager()
	if mgr == nil {
		return true
	}
	if dec := mgr.CheckExec(command); dec.Action == permission.ActionDeny {
		http.Error(w, dec.Reason, http.StatusForbidden)
		return false
	}
	return true
}

func NewServer(addr, token string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	// Use the platform temp dir instead of hardcoded /tmp — on Windows /tmp
	// doesn't exist and session storage silently dies. os.TempDir() returns:
	//   Linux:   /tmp (honors $TMPDIR)
	//   macOS:   /var/folders/.../T (honors $TMPDIR)
	//   Windows: %TEMP% (typically C:\Users\<user>\AppData\Local\Temp)
	return &Server{
		addr:           addr,
		token:          token,
		executor:       NewExecutor(),
		ptyHandler:     NewPtyHandler(),
		sessionManager: NewSessionManager(filepath.Join(os.TempDir(), "sandrpod-sessions")),
		startTime:      time.Now(),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// authMiddleware validates the Bearer token (pass-through when token is empty).
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// Handler returns the Toolbox HTTP handler, which can be mounted on any net.Listener
// (e.g. a yamux session). Also starts the session cleanup goroutine (idempotent, safe to call multiple times).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Start the session cleanup goroutine, bound to the Server's lifecycle
	s.sessionManager.StartCleanupGoroutine(s.ctx, DefaultSessionTTL, CleanupInterval)

	a := s.authMiddleware

	// Public endpoints (no authentication required)
	mux.HandleFunc("/health", s.healthHandler)

	// Authenticated endpoints
	mux.HandleFunc("/info", a(s.infoHandler))
	mux.HandleFunc("/process", a(s.processHandler))
	mux.HandleFunc("/status", a(s.statusHandler))
	mux.HandleFunc("/stream", a(s.streamHandler))

	// File operation routes
	mux.HandleFunc("/files/project-dir", a(s.projectDirHandler))
	mux.HandleFunc("/files/user-home-dir", a(s.userHomeDirHandler))
	mux.HandleFunc("/files/work-dir", a(s.workDirHandler))
	mux.HandleFunc("/files", a(s.filesHandler))
	mux.HandleFunc("/files/delete", a(s.filesDeleteHandler))
	mux.HandleFunc("/files/info", a(s.filesInfoHandler))
	mux.HandleFunc("/files/move", a(s.filesMoveHandler))
	mux.HandleFunc("/files/folder", a(s.filesFolderHandler))
	mux.HandleFunc("/files/download", a(s.filesDownloadHandler))
	mux.HandleFunc("/files/search", a(s.filesSearchHandler))
	mux.HandleFunc("/files/find", a(s.filesFindHandler))
	mux.HandleFunc("/files/replace", a(s.filesReplaceHandler))
	mux.HandleFunc("/files/permissions", a(s.filesPermissionsHandler))
	mux.HandleFunc("/files/upload", a(s.filesUploadHandler))
	mux.HandleFunc("/files/bulk-upload", a(s.filesBulkUploadHandler))

	// Port preview proxy (web services started inside the sandbox)
	mux.HandleFunc("/proxy/", a(s.proxyPortHandler))

	// E2B-compatible stateful code interpreter (run_code + contexts)
	mux.HandleFunc("/code-interpreter/execute", a(s.codeInterpreterHandler))
	mux.HandleFunc("/code-interpreter/contexts", a(s.codeContextsHandler))
	mux.HandleFunc("/code-interpreter/contexts/", a(s.codeContextByIDHandler))

	// PTY routes
	mux.HandleFunc("/pty/create", a(s.ptyCreateHandler))
	mux.HandleFunc("/pty/", a(s.ptyWsHandler))

	// Session routes
	mux.HandleFunc("/process/session", a(s.sessionHandler))
	mux.HandleFunc("/process/session/", a(s.sessionHandler))

	// Managed process table (E2B Process service: background/connect/stdin/signal/PTY).
	mux.HandleFunc("/procmgr/start", a(s.procStartHandler))
	mux.HandleFunc("/procmgr/list", a(s.procListHandler))
	mux.HandleFunc("/procmgr/stream", a(s.procStreamHandler))
	mux.HandleFunc("/procmgr/input", a(s.procInputHandler))
	mux.HandleFunc("/procmgr/signal", a(s.procSignalHandler))
	mux.HandleFunc("/procmgr/stdin-close", a(s.procStdinCloseHandler))
	mux.HandleFunc("/procmgr/resize", a(s.procResizeHandler))

	// Filesystem watch (E2B watch_dir) + resource metrics (E2B get_metrics).
	mux.HandleFunc("/watch/create", a(s.watchCreateHandler))
	mux.HandleFunc("/watch/events", a(s.watchEventsHandler))
	mux.HandleFunc("/watch/remove", a(s.watchRemoveHandler))
	mux.HandleFunc("/metrics", a(s.metricsHandler))

	// Optional MCP bridge. The bridge owns "/mcp" and any "/mcp/..." subpath
	// (manifest, tool calls). Mounted only when an MCP handler was installed
	// via SetMCPHandler; it carries its own auth (shared-secret), so it is
	// intentionally not wrapped in the toolbox authMiddleware here.
	if s.mcpHandler != nil {
		mux.Handle("/mcp", s.mcpHandler)
		mux.Handle("/mcp/", s.mcpHandler)
	}

	return mux
}

// Start starts the server on a TCP listener.
func (s *Server) Start() error {
	mux := s.Handler()

	s.server = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	return s.server.ListenAndServe()
}

// Stop shuts down the server and cancels the internal context (stopping cleanup goroutines).
func (s *Server) Stop(ctx context.Context) error {
	s.cancel()
	return s.server.Shutdown(ctx)
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	status := s.executor.HealthCheck()

	resp := HealthStatus{
		Status:    "ok",
		Docker:    status.Docker,
		Timestamp: time.Now().Unix(),
	}

	if !status.Docker {
		resp.Status = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// infoHandler GET /info — returns the sandbox runtime environment info for AI script generation
func (s *Server) infoHandler(w http.ResponseWriter, r *http.Request) {
	info := getEnvInfo()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// getEnvInfo collects runtime environment information about the sandbox container.
// OS-version / kernel-version probing is delegated to platform_{unix,windows}.go
// so the Unix-only /etc/os-release and /proc/version reads don't run on Windows.
func getEnvInfo() EnvironmentInfo {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = nativeShell()
	}

	workDir, _ := os.Getwd()
	home, _ := os.UserHomeDir() // empty on failure — consumers must nil-check

	return EnvironmentInfo{
		Arch:          runtime.GOARCH,
		OS:            runtime.GOOS,
		OSVersion:     platformOSVersion(),
		KernelVersion: platformKernelVersion(),
		Shell:         shell,
		WorkDir:       workDir,
		Home:          home,
	}
}

func (s *Server) processHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("[TOOLBOX] %s %s", r.Method, r.URL.Path)

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	var req ProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// Default timeout is 30 seconds
	timeout := 30
	if req.Timeout > 0 {
		timeout = req.Timeout
	}

	log.Printf("[TOOLBOX] execute: language=%s, timeout=%d, code=%q", req.Language, timeout, req.Code)
	if req.Timeout > 0 {
		timeout = req.Timeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	result, err := s.executor.Execute(ctx, req.Language, req.Code)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result = &ProcessResult{
				ExitCode: 124,
				Stdout:   "",
				Stderr:   fmt.Sprintf("Command timed out after %d seconds", timeout),
			}
		} else {
			http.Error(w, fmt.Sprintf("Execution error: %v", err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	requests := s.requests
	uptime := time.Since(s.startTime)
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"requests": requests,
		"uptime":   uptime.String(),
		"executor": s.executor.Stats(),
	})
}

// streamHandler handles SSE streaming execution (supports both GET query params and POST JSON body).
func (s *Server) streamHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var language, code string
	var timeout int = 30

	if r.Method == http.MethodPost {
		// POST: read parameters from the JSON body
		var req StreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
		language = req.Language
		code = req.Code
		timeout = req.Timeout
		if timeout == 0 {
			timeout = 30
		}
	} else {
		// GET: read parameters from URL query string (backward compatible)
		language = r.URL.Query().Get("language")
		code = r.URL.Query().Get("code")
		if t := r.URL.Query().Get("timeout"); t != "" {
			fmt.Sscanf(t, "%d", &timeout)
		}
	}

	if language == "" || code == "" {
		http.Error(w, "language and code are required", http.StatusBadRequest)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	// Execute with streaming output
	result, err := s.executor.ExecuteStream(ctx, language, code, func(event string, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	})

	// Send the final result event
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
	} else {
		fmt.Fprintf(w, "event: exit\ndata: %d\n\n", result.ExitCode)
	}
	flusher.Flush()
}

// ptyCreateHandler creates a new PTY session.
//
// When the executor has a permission.Manager installed, we ask the human
// for consent BEFORE spawning a shell. This is intentionally heavier than a
// per-file prompt: a PTY can do anything inside the paths the existing
// rules already allow, so we want a deliberate "yes I want to give the AI
// a shell right now" gesture each time.
func (s *Server) ptyCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	width := 80
	height := 24

	if wq := r.URL.Query().Get("width"); wq != "" {
		fmt.Sscanf(wq, "%d", &width)
	}
	if hq := r.URL.Query().Get("height"); hq != "" {
		fmt.Sscanf(hq, "%d", &height)
	}

	// Permission gate. The executor's manager is the authoritative source
	// — if there is none (legacy mode), this is a no-op and PTY proceeds
	// as before so the change is backwards-compatible for existing users.
	if mgr := s.executor.PermissionManager(); mgr != nil {
		sandboxName := r.URL.Query().Get("sandbox")
		if sandboxName == "" {
			// We're inside agent mode where sandbox name is implicit; use
			// hostname as a stable label that the dialog can show.
			sandboxName, _ = os.Hostname()
		}
		dec := mgr.CheckPTY(r.Context(), sandboxName, sessionFromContext(r.Context()))
		if dec.Action == permission.ActionDeny {
			http.Error(w, dec.Reason, http.StatusForbidden)
			return
		}
	}

	sessionID, err := s.ptyHandler.CreateSession(width, height)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create PTY session: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"session_id": sessionID,
	})
}

// ptyWsHandler handles PTY WebSocket connections.
func (s *Server) ptyWsHandler(w http.ResponseWriter, r *http.Request) {
	// Path: /pty/{sessionId}
	path := r.URL.Path[len("/pty/"):]
	if path == "" {
		http.Error(w, "session ID is required", http.StatusBadRequest)
		return
	}

	// Upgrade the HTTP connection to WebSocket
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("WebSocket upgrade failed: %v\n", err)
		return
	}
	defer conn.Close()

	// Handle the WebSocket connection
	if err := s.ptyHandler.HandleWebSocket(conn, path); err != nil {
		fmt.Printf("PTY handler error: %v\n", err)
	}
}

// projectDirHandler returns the project directory path.
func (s *Server) projectDirHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := s.executor.GetProjectDir()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": path})
}

// userHomeDirHandler returns the user's home directory path.
func (s *Server) userHomeDirHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := s.executor.GetUserHomeDir()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": path})
}

// workDirHandler returns the current working directory path.
func (s *Server) workDirHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := s.executor.GetWorkDir()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": path})
}

// filesHandler lists files in the given directory.
func (s *Server) filesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		path = s.executor.GetProjectDir()
	}

	files, err := s.executor.ListFiles(r.Context(), path)
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			writeError(w, err)
		} else if strings.Contains(err.Error(), "failed to read directory") {
			http.Error(w, fmt.Sprintf("Cannot read directory '%s': %s", path, err.Error()), http.StatusBadRequest)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"path":  path,
		"files": files,
	})
}

// filesDeleteHandler deletes a file or directory.
func (s *Server) filesDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	if err := s.executor.DeleteFile(r.Context(), path); err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"path":    path,
	})
}

// filesInfoHandler returns metadata for a file.
func (s *Server) filesInfoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	info, err := s.executor.GetFileInfo(r.Context(), path)
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			writeError(w, err)
		} else if strings.Contains(err.Error(), "no such file") {
			http.Error(w, fmt.Sprintf("File not found: '%s'", path), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// filesMoveHandler moves or renames a file or directory.
func (s *Server) filesMoveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	source := r.URL.Query().Get("source")
	destination := r.URL.Query().Get("destination")
	if source == "" || destination == "" {
		http.Error(w, "source and destination are required", http.StatusBadRequest)
		return
	}

	if err := s.executor.MoveFile(r.Context(), source, destination); err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success":     true,
		"source":      source,
		"destination": destination,
	})
}

// filesFolderHandler creates a directory.
func (s *Server) filesFolderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	if err := s.executor.CreateFolder(r.Context(), path); err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"path":    path,
	})
}

// filesDownloadHandler streams a file to the client.
//
// Uses http.ServeContent so large files don't have to fit in memory; it also
// gives clients Range, ETag, and Last-Modified support for free. Permission
// gating is applied via resolveAndAuthorize before we open the file so the
// consent prompt still fires before any disk I/O happens.
func (s *Server) filesDownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	safe, err := s.executor.resolveAndAuthorize(r.Context(), path, permission.ModeRead, "files.download")
	if err != nil {
		writeError(w, err)
		return
	}

	f, err := os.Open(safe)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, fmt.Sprintf("File not found: %q", path), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("failed to open file: %v", err), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to stat file: %v", err), http.StatusInternalServerError)
		return
	}
	if fi.IsDir() {
		http.Error(w, "path is a directory; use /files for listings", http.StatusBadRequest)
		return
	}

	name := filepath.Base(safe)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	// ServeContent will set Content-Length, handle Range/If-Modified-Since,
	// and stream straight from the file descriptor — no memory buffering.
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// filesSearchHandler searches for files matching a glob pattern.
func (s *Server) filesSearchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		http.Error(w, "pattern is required", http.StatusBadRequest)
		return
	}

	result, err := s.executor.SearchFiles(r.Context(), path, pattern)
	if err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// filesFindHandler searches file contents for a pattern.
func (s *Server) filesFindHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		http.Error(w, "pattern is required", http.StatusBadRequest)
		return
	}

	matches, err := s.executor.FindInFiles(r.Context(), path, pattern)
	if err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(matches)
}

// ReplaceRequest is the request payload for a text replacement operation.
type ReplaceRequest struct {
	Files    []string `json:"files"`
	NewValue string   `json:"newValue"`
	Pattern  string   `json:"pattern"`
}

// filesReplaceHandler replaces text in files.
func (s *Server) filesReplaceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ReplaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	results, err := s.executor.ReplaceInFiles(r.Context(), req.Files, req.Pattern, req.NewValue)
	if err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// filesPermissionsHandler sets file ownership and permissions.
func (s *Server) filesPermissionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	owner := r.URL.Query().Get("owner")
	group := r.URL.Query().Get("group")
	modeStr := r.URL.Query().Get("mode")

	mode := os.FileMode(0644)
	if modeStr != "" {
		fmt.Sscanf(modeStr, "%o", &mode)
	}

	if err := s.executor.SetFilePermissions(r.Context(), path, owner, group, mode); err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"path":    path,
	})
}

// filesUploadHandler handles single file uploads.
//
// Three things this implementation is careful about that the previous version
// got wrong:
//  1. Path separators — normalize via filepath.FromSlash before calling Stat,
//     so a Windows agent can handle a client that sends "/c/Users/x/dir".
//  2. Directory traversal — header.Filename is untrusted input. Strip it down
//     to filepath.Base so an upload named "../../etc/passwd" lands as "passwd"
//     inside the requested directory.
//  3. Permission gate — use resolveAndAuthorize (not resolveSafePath) so the
//     human consent prompt fires for uploads outside workDir.
func (s *Server) filesUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rawPath := r.URL.Query().Get("path")
	if rawPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	path := filepath.FromSlash(rawPath)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, fmt.Sprintf("failed to parse multipart form: %v", err), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Decide destination. If the caller pointed at a directory we append the
	// (sanitized) original filename; otherwise we treat path as the literal
	// destination file. filepath.Base strips any directory components from
	// header.Filename, blocking "../../etc/passwd"-style traversal attacks.
	destPath := path
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		destPath = filepath.Join(path, filepath.Base(header.Filename))
	}

	safeDest, err := s.executor.resolveAndAuthorize(r.Context(), destPath, permission.ModeWrite, "files.upload")
	if err != nil {
		writeError(w, err)
		return
	}

	// Stream the upload to disk instead of buffering the whole body in memory.
	// 32 MiB form limit above only bounds the parser; the file we create here
	// could legitimately be larger if the client uses chunked encoding.
	out, err := os.OpenFile(safeDest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create file: %v", err), http.StatusInternalServerError)
		return
	}
	n, copyErr := io.Copy(out, file)
	if closeErr := out.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		http.Error(w, fmt.Sprintf("failed to write file: %v", copyErr), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"path":    safeDest,
		"name":    filepath.Base(header.Filename),
		"size":    n,
	})
}

// filesBulkUploadHandler handles bulk file uploads.
//
// Same hardening as filesUploadHandler: FromSlash on the target path, Base
// on each filename (so per-field keys containing "../" can't escape), and
// every destination passes through resolveAndAuthorize for permission
// gating. Each file is streamed via io.Copy instead of io.ReadAll so a
// 4 GiB upload doesn't OOM the agent process.
func (s *Server) filesBulkUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rawPath := r.URL.Query().Get("path")
	if rawPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	path := filepath.FromSlash(rawPath)

	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, fmt.Sprintf("failed to parse multipart form: %v", err), http.StatusBadRequest)
		return
	}

	results := make([]map[string]any, 0)

	for filename, fileHeader := range r.MultipartForm.File {
		for _, header := range fileHeader {
			// Prefer the header's own filename (set by the uploading client)
			// over the form-field key, then strip path components either way.
			displayName := header.Filename
			if displayName == "" {
				displayName = filename
			}
			safeName := filepath.Base(displayName)

			file, err := header.Open()
			if err != nil {
				results = append(results, map[string]any{
					"name":    safeName,
					"success": false,
					"error":   err.Error(),
				})
				continue
			}

			destPath := filepath.Join(path, safeName)
			safeDest, err := s.executor.resolveAndAuthorize(r.Context(), destPath, permission.ModeWrite, "files.bulk_upload")
			if err != nil {
				file.Close()
				results = append(results, map[string]any{
					"name":    safeName,
					"success": false,
					"error":   err.Error(),
				})
				continue
			}

			out, err := os.OpenFile(safeDest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				file.Close()
				results = append(results, map[string]any{
					"name":    safeName,
					"success": false,
					"error":   err.Error(),
				})
				continue
			}
			n, copyErr := io.Copy(out, file)
			file.Close()
			if closeErr := out.Close(); closeErr != nil && copyErr == nil {
				copyErr = closeErr
			}
			if copyErr != nil {
				results = append(results, map[string]any{
					"name":    safeName,
					"success": false,
					"error":   copyErr.Error(),
				})
				continue
			}

			results = append(results, map[string]any{
				"name":    safeName,
				"success": true,
				"path":    safeDest,
				"size":    n,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"results": results,
	})
}
