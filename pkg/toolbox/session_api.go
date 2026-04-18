// Copyright 2024 SandrPod
// Session API - HTTP 路由处理

package toolbox

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// sessionHandler 处理所有 /process/session 相关路由
func (s *Server) sessionHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// 打印所有请求
	log.Printf("[TOOLBOX] %s %s", r.Method, path)

	// POST /process/session 或 /process/session/ - 创建 Session
	if r.Method == http.MethodPost && (path == "/process/session" || path == "/process/session/") {
		s.sessionCreateHandler(w, r)
		return
	}

	// GET /process/session 或 /process/session/ - 列出所有 Session
	if r.Method == http.MethodGet && (path == "/process/session" || path == "/process/session/") {
		s.sessionListHandler(w, r)
		return
	}

	// DELETE /process/session/{id} - 删除 Session
	if r.Method == http.MethodDelete && strings.HasPrefix(path, "/process/session/") {
		sessionId := strings.TrimPrefix(path, "/process/session/")
		// 确保不是子路径
		if !strings.Contains(sessionId, "/") {
			s.sessionDeleteHandler(w, r, sessionId)
			return
		}
	}

	// GET /process/session/{id} 或 /process/session/{sandboxName}/{id} - 获取 Session
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/process/session/") {
		rest := strings.TrimPrefix(path, "/process/session/")
		// 支持两种格式：
		// 1. /process/session/{sessionId} - 直接获取
		// 2. /process/session/{sandboxName}/{sessionId} - 通过 Poder 代理时包含 sandboxName
		if strings.Contains(rest, "/") {
			// 格式: {sandboxName}/{sessionId}
			parts := strings.Split(rest, "/")
			sessionId := parts[len(parts)-1]
			if sessionId != "" {
				s.sessionGetHandler(w, r, sessionId)
				return
			}
		} else {
			// 格式: {sessionId}
			s.sessionGetHandler(w, r, rest)
			return
		}
	}

	// POST /process/session/{sandboxName}/{sessionId}/exec - 执行命令
	if r.Method == http.MethodPost && strings.HasPrefix(path, "/process/session/") {
		rest := strings.TrimPrefix(path, "/process/session/")
		if strings.HasSuffix(rest, "/exec") {
			// URL 格式: /process/session/{sandboxName}/{sessionId}/exec
			// 或者: /process/session/{sessionId}/exec
			rest = strings.TrimSuffix(rest, "/exec")
			parts := strings.Split(rest, "/")
			var sessionId string
			if len(parts) >= 2 {
				// 格式: {sandboxName}/{sessionId} 或 {sessionId}
				sessionId = parts[len(parts)-1]
			} else {
				sessionId = rest
			}
			if sessionId == "" {
				http.Error(w, "session_id is required", http.StatusBadRequest)
				return
			}
			s.sessionExecHandler(w, r, sessionId)
			return
		}
	}

	// GET /process/session/{id}/command/{cmdId} - 获取命令结果
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/process/session/") {
		rest := strings.TrimPrefix(path, "/process/session/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 3 && parts[1] == "command" {
			sessionId := parts[0]
			cmdId := parts[2]
			s.sessionCommandHandler(w, r, sessionId, cmdId)
			return
		}
	}

	http.NotFound(w, r)
}

// sessionCreateHandler 创建 Session
func (s *Server) sessionCreateHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空，使用自动生成的 ID
		req = CreateSessionRequest{}
	}

	session, err := s.sessionManager.Create(req.SessionId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(session.ToDTO())
}

// sessionListHandler 列出所有 Session
func (s *Server) sessionListHandler(w http.ResponseWriter, r *http.Request) {
	sessions := s.sessionManager.List()

	dtos := make([]*SessionDTO, 0, len(sessions))
	for _, session := range sessions {
		dtos = append(dtos, session.ToDTO())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dtos)
}

// sessionDeleteHandler 删除 Session
func (s *Server) sessionDeleteHandler(w http.ResponseWriter, r *http.Request, sessionId string) {
	err := s.sessionManager.Delete(sessionId)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// sessionGetHandler 获取 Session 详情
func (s *Server) sessionGetHandler(w http.ResponseWriter, r *http.Request, sessionId string) {
	session, ok := s.sessionManager.Get(sessionId)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session.ToDTO())
}

// sessionExecHandler 执行命令
func (s *Server) sessionExecHandler(w http.ResponseWriter, r *http.Request, sessionId string) {
	var req SessionExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	// 如果请求体中指定了 session_id，优先使用
	if req.SessionId != "" {
		sessionId = req.SessionId
	}

	resp, err := s.sessionManager.Execute(sessionId, "", req.Command, req.Async)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if req.Async {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

// sessionCommandHandler 获取命令结果
func (s *Server) sessionCommandHandler(w http.ResponseWriter, r *http.Request, sessionId, cmdId string) {
	cmd, err := s.sessionManager.GetCommand(sessionId, cmdId)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 读取输出
	output, err := s.sessionManager.GetCommandOutput(sessionId, cmdId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := struct {
		CommandId string  `json:"cmd_id"`
		Command   string  `json:"command"`
		ExitCode  *int    `json:"exit_code,omitempty"`
		Output    string  `json:"output"`
	}{
		CommandId: cmd.ID,
		Command:   cmd.Command,
		ExitCode:  cmd.ExitCode,
		Output:    string(output),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// sessionLogsHandler 获取命令日志 (SSE 流式)
func (s *Server) sessionLogsHandler(w http.ResponseWriter, r *http.Request, sessionId, cmdId string) {
	// 设置 SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	session, ok := s.sessionManager.Get(sessionId)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	session.mu.RLock()
	cmd, ok := session.Commands[cmdId]
	session.mu.RUnlock()

	if !ok {
		http.Error(w, "command not found", http.StatusNotFound)
		return
	}

	// 流式读取日志
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// 发送初始事件
	fmt.Fprintf(w, "data: {\"status\": \"running\"}\n\n")
	flusher.Flush()

	// 轮询直到命令完成
	for {
		if cmd.ExitCode != nil {
			break
		}

		// 读取当前日志
		content, err := os.ReadFile(cmd.LogFile)
		if err == nil && len(content) > 0 {
			fmt.Fprintf(w, "data: %s\n\n", content)
			flusher.Flush()
		}

		time.Sleep(100 * time.Millisecond)
	}

	// 发送完成事件
	fmt.Fprintf(w, "data: {\"status\": \"completed\", \"exit_code\": %d}\n\n", *cmd.ExitCode)
	flusher.Flush()
}
