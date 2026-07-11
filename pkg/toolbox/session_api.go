// Copyright 2024 SandrPod
// Session API - HTTP route handlers

package toolbox

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
)

// sessionHandler dispatches all /process/session requests to the appropriate sub-handler.
func (s *Server) sessionHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Log every incoming request.
	log.Printf("[TOOLBOX] %s %s", r.Method, path)

	// POST /process/session or /process/session/ - create a session
	if r.Method == http.MethodPost && (path == "/process/session" || path == "/process/session/") {
		s.sessionCreateHandler(w, r)
		return
	}

	// GET /process/session or /process/session/ - list all sessions
	if r.Method == http.MethodGet && (path == "/process/session" || path == "/process/session/") {
		s.sessionListHandler(w, r)
		return
	}

	// DELETE /process/session/{id} - delete a session
	if r.Method == http.MethodDelete && strings.HasPrefix(path, "/process/session/") {
		sessionId := strings.TrimPrefix(path, "/process/session/")
		// Ensure this is not a sub-path.
		if !strings.Contains(sessionId, "/") {
			s.sessionDeleteHandler(w, r, sessionId)
			return
		}
	}

	// GET /process/session/{id}/command/{cmdId} - retrieve command result.
	// MUST precede the generic GET branch below, which would otherwise treat
	// "{id}/command/{cmdId}" as "{sandboxName}/{sessionId}" and swallow it.
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/process/session/") {
		rest := strings.TrimPrefix(path, "/process/session/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 3 && parts[1] == "command" {
			s.sessionCommandHandler(w, r, parts[0], parts[2])
			return
		}
	}

	// GET /process/session/{id} or /process/session/{sandboxName}/{id} - get a session
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/process/session/") {
		rest := strings.TrimPrefix(path, "/process/session/")
		// Two URL formats are supported:
		// 1. /process/session/{sessionId}           - direct lookup
		// 2. /process/session/{sandboxName}/{sessionId} - proxied through Poder with sandbox name prefix
		if strings.Contains(rest, "/") {
			// Format: {sandboxName}/{sessionId}
			parts := strings.Split(rest, "/")
			sessionId := parts[len(parts)-1]
			if sessionId != "" {
				s.sessionGetHandler(w, r, sessionId)
				return
			}
		} else {
			// Format: {sessionId}
			s.sessionGetHandler(w, r, rest)
			return
		}
	}

	// POST /process/session/{sandboxName}/{sessionId}/exec - execute a command
	if r.Method == http.MethodPost && strings.HasPrefix(path, "/process/session/") {
		rest := strings.TrimPrefix(path, "/process/session/")
		if rest, ok := strings.CutSuffix(rest, "/exec"); ok {
			// URL formats:
			//   /process/session/{sandboxName}/{sessionId}/exec
			//   /process/session/{sessionId}/exec
			parts := strings.Split(rest, "/")
			var sessionId string
			if len(parts) >= 2 {
				// Format: {sandboxName}/{sessionId} or just {sessionId}
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

	http.NotFound(w, r)
}

// sessionCreateHandler creates a new session.
func (s *Server) sessionCreateHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Fall back to an auto-generated ID when the body is empty or invalid.
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

// sessionListHandler returns all active sessions.
func (s *Server) sessionListHandler(w http.ResponseWriter, _ *http.Request) {
	sessions := s.sessionManager.List()

	dtos := make([]*SessionDTO, 0, len(sessions))
	for _, session := range sessions {
		dtos = append(dtos, session.ToDTO())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dtos)
}

// sessionDeleteHandler deletes a session by ID.
func (s *Server) sessionDeleteHandler(w http.ResponseWriter, r *http.Request, sessionId string) {
	err := s.sessionManager.Delete(sessionId)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// sessionGetHandler returns the details for a single session.
func (s *Server) sessionGetHandler(w http.ResponseWriter, r *http.Request, sessionId string) {
	session, ok := s.sessionManager.Get(sessionId)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session.ToDTO())
}

// sessionExecHandler executes a command within an existing session.
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

	// If the request body includes a session_id, it takes precedence over the URL parameter.
	if req.SessionId != "" {
		sessionId = req.SessionId
	}

	if !s.gateExec(w, req.Command) {
		return
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

// sessionCommandHandler returns the result and output for a previously executed command.
func (s *Server) sessionCommandHandler(w http.ResponseWriter, r *http.Request, sessionId, cmdId string) {
	cmd, err := s.sessionManager.GetCommand(sessionId, cmdId)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Read the captured output for this command.
	output, err := s.sessionManager.GetCommandOutput(sessionId, cmdId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := struct {
		CommandId string `json:"cmd_id"`
		Command   string `json:"command"`
		ExitCode  *int   `json:"exit_code,omitempty"`
		Output    string `json:"output"`
	}{
		CommandId: cmd.ID,
		Command:   cmd.Command,
		ExitCode:  cmd.ExitCode,
		Output:    string(output),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
