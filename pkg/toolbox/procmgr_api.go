// Copyright 2026 SandrPod Contributors
// HTTP surface for the managed process table (/procmgr/*). The e2bcompat
// gateway proxies the E2B Process service onto these endpoints:
//
//	POST /procmgr/start        {cmd,args,envs,cwd,tag,pty,rows,cols} → {pid}
//	GET  /procmgr/list                                              → [ProcInfo]
//	GET  /procmgr/stream?pid=N                → chunked NDJSON of ProcEvent
//	POST /procmgr/input        {pid,data(base64),pty}
//	POST /procmgr/signal       {pid,signal}
//	POST /procmgr/stdin-close  {pid}
//	POST /procmgr/resize       {pid,rows,cols}

package toolbox

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"syscall"
)

func (s *Server) procStartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Cmd  string            `json:"cmd"`
		Args []string          `json:"args"`
		Envs map[string]string `json:"envs"`
		Cwd  string            `json:"cwd"`
		Tag  string            `json:"tag"`
		PTY  bool              `json:"pty"`
		Rows uint16            `json:"rows"`
		Cols uint16            `json:"cols"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.gateExec(w, strings.TrimSpace(req.Cmd+" "+strings.Join(req.Args, " "))) {
		return
	}
	pid, err := s.procManager().Start(ProcStartConfig{
		Cmd: req.Cmd, Args: req.Args, Envs: req.Envs, Cwd: req.Cwd,
		Tag: req.Tag, PTY: req.PTY, Rows: req.Rows, Cols: req.Cols,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]uint32{"pid": pid})
}

func (s *Server) procListHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.procManager().List())
}

// procStreamHandler streams a process's output as newline-delimited ProcEvent
// JSON, flushing each event so the gateway can relay it live.
func (s *Server) procStreamHandler(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseUint(r.URL.Query().Get("pid"), 10, 32)
	if err != nil {
		http.Error(w, "invalid pid", http.StatusBadRequest)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	found, streamErr := s.procManager().Stream(uint32(pid), func(ev ProcEvent) error {
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if !found {
		http.Error(w, "process not found", http.StatusNotFound)
		return
	}
	_ = streamErr // client disconnect ends the stream; nothing to report
}

func (s *Server) procInputHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PID  uint32 `json:"pid"`
		Data []byte `json:"data"`
		PTY  bool   `json:"pty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !s.procManager().SendInput(req.PID, req.Data, req.PTY) {
		http.Error(w, "process not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) procSignalHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PID    uint32 `json:"pid"`
		Signal int    `json:"signal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	sig := syscall.Signal(req.Signal)
	if req.Signal == 0 {
		sig = syscall.SIGKILL
	}
	if !s.procManager().Signal(req.PID, sig) {
		http.Error(w, "process not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) procStdinCloseHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PID uint32 `json:"pid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !s.procManager().CloseStdin(req.PID) {
		http.Error(w, "process not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) procResizeHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PID  uint32 `json:"pid"`
		Rows uint16 `json:"rows"`
		Cols uint16 `json:"cols"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !s.procManager().Resize(req.PID, req.Rows, req.Cols) {
		http.Error(w, "process not found or not a PTY", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
