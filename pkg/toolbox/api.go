// Copyright 2024 SandrPod
// Toolbox API - 代码执行服务

package toolbox

import (
	"context"
	"encoding/json"
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
)

// StreamRequest 流式执行请求 (query parameters)
type StreamRequest struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Timeout  int    `json:"timeout"`
}

// ProcessRequest 进程执行请求
type ProcessRequest struct {
	Language string `json:"language"` // python, node, bash
	Code    string `json:"code"`
	Timeout int    `json:"timeout"` // seconds
}

// ProcessResult 进程执行结果
type ProcessResult struct {
	ExitCode  int       `json:"exit_code"`
	Stdout    string    `json:"stdout"`
	Stderr    string    `json:"stderr"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// HealthStatus 健康状态
type HealthStatus struct {
	Status    string `json:"status"`
	Docker    bool   `json:"docker"`
	Timestamp int64  `json:"timestamp"`
}

// EnvironmentInfo 容器运行环境信息（供 AI 生成脚本时参考）
type EnvironmentInfo struct {
	Arch          string `json:"arch"`           // e.g. amd64, arm64
	OS            string `json:"os"`             // e.g. linux
	OSVersion     string `json:"os_version"`     // e.g. Ubuntu 22.04.3 LTS
	KernelVersion string `json:"kernel_version"` // e.g. 5.15.0-91-generic
	Shell         string `json:"shell"`          // e.g. /bin/bash
	WorkDir       string `json:"work_dir"`       // default working directory
}

// Server Toolbox HTTP 服务器
type Server struct {
	addr           string
	executor      *Executor
	ptyHandler    *PtyHandler
	sessionManager *SessionManager
	server        *http.Server
	mu            sync.RWMutex
	requests      int64
	startTime     time.Time
}

const CleanupTimeout = 5 * time.Second

// NewServer 创建 Toolbox 服务器
func NewServer(addr string) *Server {
	return &Server{
		addr:           addr,
		executor:       NewExecutor(),
		ptyHandler:     NewPtyHandler(),
		sessionManager: NewSessionManager("/tmp/sandrpod-sessions"),
		startTime:      time.Now(),
	}
}

// Handler 返回 Toolbox 的 HTTP handler，可挂载到任意 net.Listener（如 yamux session）。
// 同时启动 session 清理 goroutine（幂等，多次调用安全）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 启动 session 清理 goroutine
	s.sessionManager.StartCleanupGoroutine(DefaultSessionTTL, CleanupInterval)

	// API 路由
	mux.HandleFunc("/health", s.healthHandler)
	mux.HandleFunc("/info", s.infoHandler)
	mux.HandleFunc("/process", s.processHandler)
	mux.HandleFunc("/processAsync", s.processAsyncHandler)
	mux.HandleFunc("/status", s.statusHandler)
	mux.HandleFunc("/stream", s.streamHandler)

	// 文件操作路由
	mux.HandleFunc("/files/project-dir", s.projectDirHandler)
	mux.HandleFunc("/files/user-home-dir", s.userHomeDirHandler)
	mux.HandleFunc("/files/work-dir", s.workDirHandler)
	mux.HandleFunc("/files", s.filesHandler)
	mux.HandleFunc("/files/delete", s.filesDeleteHandler)
	mux.HandleFunc("/files/info", s.filesInfoHandler)
	mux.HandleFunc("/files/move", s.filesMoveHandler)
	mux.HandleFunc("/files/folder", s.filesFolderHandler)
	mux.HandleFunc("/files/download", s.filesDownloadHandler)
	mux.HandleFunc("/files/search", s.filesSearchHandler)
	mux.HandleFunc("/files/find", s.filesFindHandler)
	mux.HandleFunc("/files/replace", s.filesReplaceHandler)
	mux.HandleFunc("/files/permissions", s.filesPermissionsHandler)
	mux.HandleFunc("/files/upload", s.filesUploadHandler)
	mux.HandleFunc("/files/bulk-upload", s.filesBulkUploadHandler)

	// PTY 路由
	mux.HandleFunc("/pty/create", s.ptyCreateHandler)
	mux.HandleFunc("/pty/", s.ptyWsHandler)

	// Session 路由
	mux.HandleFunc("/process/session", s.sessionHandler)
	mux.HandleFunc("/process/session/", s.sessionHandler)

	return mux
}

// Start 启动服务器（TCP 监听）
func (s *Server) Start() error {
	mux := s.Handler()

	s.server = &http.Server{
		Addr:         s.addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	return s.server.ListenAndServe()
}

// Stop 停止服务器
func (s *Server) Stop(ctx context.Context) error {
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

// infoHandler GET /info — 返回容器运行环境信息，供 AI 生成可执行脚本时参考
func (s *Server) infoHandler(w http.ResponseWriter, r *http.Request) {
	info := getEnvInfo()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// getEnvInfo 收集容器运行环境信息
func getEnvInfo() EnvironmentInfo {
	osVersion := runtime.GOOS
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if val, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				osVersion = strings.Trim(val, `"`)
				break
			}
		}
	}

	kernelVersion := ""
	if data, err := os.ReadFile("/proc/version"); err == nil {
		fields := strings.Fields(strings.TrimSpace(string(data)))
		// "Linux version 5.15.0-91-generic ..."
		if len(fields) >= 3 {
			kernelVersion = fields[2]
		}
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	workDir, _ := os.Getwd()

	return EnvironmentInfo{
		Arch:          runtime.GOARCH,
		OS:            runtime.GOOS,
		OSVersion:     osVersion,
		KernelVersion: kernelVersion,
		Shell:         shell,
		WorkDir:       workDir,
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

	// 默认超时 30 秒
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

func (s *Server) processAsyncHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// 返回任务 ID (简化实现，实际应该用队列)
	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())

	// 后台执行
	go func() {
		timeout := 30
		if req.Timeout > 0 {
			timeout = req.Timeout
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()

		s.executor.Execute(ctx, req.Language, req.Code)
		// 实际应该更新任务状态到 Redis/数据库
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"task_id": taskID})
}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	requests := s.requests
	uptime := time.Since(s.startTime)
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"requests":  requests,
		"uptime":    uptime.String(),
		"executor":  s.executor.Stats(),
	})
}

// streamHandler SSE流式执行 (支持 GET URL参数 和 POST JSON body)
func (s *Server) streamHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var language, code string
	var timeout int = 30

	if r.Method == http.MethodPost {
		// POST: 从 JSON body 读取
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
		// GET: 从 URL 参数读取 (保持向后兼容)
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

	// 设置SSE headers
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

	// 使用streaming执行
	result, err := s.executor.ExecuteStream(ctx, language, code, func(event string, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	})

	// 发送最终结果
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
	} else {
		fmt.Fprintf(w, "event: exit\ndata: %d\n\n", result.ExitCode)
	}
	flusher.Flush()
}

// ptyCreateHandler 创建 PTY 会话
func (s *Server) ptyCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	width := 80
	height := 24

	if w := r.URL.Query().Get("width"); w != "" {
		fmt.Sscanf(w, "%d", &width)
	}
	if h := r.URL.Query().Get("height"); h != "" {
		fmt.Sscanf(h, "%d", &height)
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

// ptyWsHandler PTY WebSocket 处理
func (s *Server) ptyWsHandler(w http.ResponseWriter, r *http.Request) {
	// 路径: /pty/{sessionId}
	path := r.URL.Path[len("/pty/"):]
	if path == "" {
		http.Error(w, "session ID is required", http.StatusBadRequest)
		return
	}

	// 升级为 WebSocket
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

	// 处理 WebSocket 连接
	if err := s.ptyHandler.HandleWebSocket(conn, path); err != nil {
		fmt.Printf("PTY handler error: %v\n", err)
	}
}

// projectDirHandler 获取项目目录
func (s *Server) projectDirHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := s.executor.GetProjectDir()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": path})
}

// userHomeDirHandler 获取用户 home 目录
func (s *Server) userHomeDirHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := s.executor.GetUserHomeDir()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": path})
}

// workDirHandler 获取工作目录
func (s *Server) workDirHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := s.executor.GetWorkDir()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": path})
}

// filesHandler 文件列表/信息
func (s *Server) filesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		path = s.executor.GetProjectDir()
	}

	files, err := s.executor.ListFiles(path)
	if err != nil {
		errMsg := err.Error()
		// 根据错误类型返回适当的状态码
		if strings.Contains(errMsg, "denied") || strings.Contains(errMsg, "permission") {
			http.Error(w, fmt.Sprintf("Access denied: cannot access '%s'. Sandbox files are restricted to the project directory.", path), http.StatusForbidden)
		} else if strings.Contains(errMsg, "failed to read directory") {
			http.Error(w, fmt.Sprintf("Cannot read directory '%s': %s", path, errMsg), http.StatusBadRequest)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"path":  path,
		"files": files,
	})
}

// filesDeleteHandler 删除文件/目录
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

	if err := s.executor.DeleteFile(path); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "cannot delete") || strings.Contains(errMsg, "denied") {
			http.Error(w, errMsg, http.StatusForbidden)
		} else {
			http.Error(w, errMsg, http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"path":    path,
	})
}

// filesInfoHandler 获取文件信息
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

	info, err := s.executor.GetFileInfo(path)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "denied") || strings.Contains(errMsg, "permission") {
			http.Error(w, fmt.Sprintf("Access denied: cannot access '%s'", path), http.StatusForbidden)
		} else if strings.Contains(errMsg, "no such file") {
			http.Error(w, fmt.Sprintf("File not found: '%s'", path), http.StatusNotFound)
		} else {
			http.Error(w, errMsg, http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// filesMoveHandler 移动或重命名文件/目录
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

	if err := s.executor.MoveFile(source, destination); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "denied") || strings.Contains(errMsg, "permission") {
			http.Error(w, fmt.Sprintf("Access denied: cannot move '%s'", source), http.StatusForbidden)
		} else {
			http.Error(w, errMsg, http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"source":     source,
		"destination": destination,
	})
}

// filesFolderHandler 创建目录
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

	if err := s.executor.CreateFolder(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"path":    path,
	})
}

// filesDownloadHandler 下载文件
func (s *Server) filesDownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	data, err := s.executor.DownloadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 设置下载 headers
	filename := filepath.Base(path)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}

// filesSearchHandler 搜索文件 (glob模式)
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

	result, err := s.executor.SearchFiles(path, pattern)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// filesFindHandler 文件内容搜索
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

	matches, err := s.executor.FindInFiles(path, pattern)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(matches)
}

// ReplaceRequest 文本替换请求
type ReplaceRequest struct {
	Files    []string `json:"files"`
	NewValue string   `json:"newValue"`
	Pattern  string   `json:"pattern"`
}

// filesReplaceHandler 文本替换
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

	results, err := s.executor.ReplaceInFiles(req.Files, req.Pattern, req.NewValue)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// filesPermissionsHandler 设置文件权限
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

	if err := s.executor.SetFilePermissions(path, owner, group, mode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"path":    path,
	})
}

// filesUploadHandler 上传文件
func (s *Server) filesUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

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

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read file: %v", err), http.StatusInternalServerError)
		return
	}

	// 如果 path 是目录，使用原始文件名
	destPath := path
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		destPath = filepath.Join(path, header.Filename)
	}

	if err := os.WriteFile(destPath, data, 0644); err != nil {
		http.Error(w, fmt.Sprintf("failed to write file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"path":    destPath,
		"name":    header.Filename,
		"size":    len(data),
	})
}

// filesBulkUploadHandler 批量上传文件
func (s *Server) filesBulkUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, fmt.Sprintf("failed to parse multipart form: %v", err), http.StatusBadRequest)
		return
	}

	results := make([]map[string]interface{}, 0)

	for filename, fileHeader := range r.MultipartForm.File {
		for _, header := range fileHeader {
			file, err := header.Open()
			if err != nil {
				results = append(results, map[string]interface{}{
					"name":    filename,
					"success": false,
					"error":   err.Error(),
				})
				continue
			}

			data, err := io.ReadAll(file)
			file.Close()
			if err != nil {
				results = append(results, map[string]interface{}{
					"name":    filename,
					"success": false,
					"error":   err.Error(),
				})
				continue
			}

			destPath := filepath.Join(path, filename)
			if err := os.WriteFile(destPath, data, 0644); err != nil {
				results = append(results, map[string]interface{}{
					"name":    filename,
					"success": false,
					"error":   err.Error(),
				})
				continue
			}

			results = append(results, map[string]interface{}{
				"name":    filename,
				"success": true,
				"path":    destPath,
				"size":    len(data),
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"results": results,
	})
}
