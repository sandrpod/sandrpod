// Copyright 2024 SandrPod
// envd surface: the per-sandbox daemon the E2B SDK reaches at
// <port>-<sandboxID>.<domain>. We terminate it centrally at the server and
// translate onto an EnvdBackend (the toolbox, over the tunnel).
//
// envd exposes:
//   - connect-rpc Filesystem service (Stat/MakeDir/Move/ListDir/Remove) — unary
//   - connect-rpc Process service (List/Start/...) — Start is server-streaming
//   - plain HTTP for file CONTENT read/write (proto has no Read/Write RPC)
//
// Connect unary maps to POST /<pkg>.<Service>/<Method> with a JSON body and a
// JSON reply; errors use an HTTP status + {"code","message"}. Field names use
// the proto3 JSON (lowerCamelCase) mapping so the SDK's generated client
// decodes them unchanged.

package e2bcompat

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// EnvdBackend is what the envd surface needs from a specific sandbox. The
// SandrPod server implements it by proxying to the sandbox's toolbox over the
// tunnel; tests use an in-memory fake.
type EnvdBackend interface {
	Stat(sandboxID, path string) (EntryInfo, error)
	MakeDir(sandboxID, path string) (EntryInfo, error)
	Move(sandboxID, src, dst string) (EntryInfo, error)
	ListDir(sandboxID, path string, depth uint32) ([]EntryInfo, error)
	Remove(sandboxID, path string) error
	ReadFile(sandboxID, path string) ([]byte, error)
	WriteFile(sandboxID, path string, data []byte) (EntryInfo, error)
	// StartProcess runs a command to completion and returns its output. The
	// streaming Start RPC is synthesized from this (data frame + end frame).
	StartProcess(sandboxID string, cfg ProcessConfig) (ProcResult, error)
}

// ---- proto3-JSON message shapes (filesystem) ----

type EntryInfo struct {
	Name          string     `json:"name"`
	Type          FileType   `json:"type"`
	Path          string     `json:"path"`
	Size          int64      `json:"size"`
	Mode          uint32     `json:"mode"`
	Permissions   string     `json:"permissions"`
	Owner         string     `json:"owner,omitempty"`
	Group         string     `json:"group,omitempty"`
	ModifiedTime  *time.Time `json:"modifiedTime,omitempty"`
	SymlinkTarget *string    `json:"symlinkTarget,omitempty"`
}

// FileType mirrors the proto enum (JSON uses the enum value NAME).
type FileType string

const (
	FileTypeUnspecified FileType = "FILE_TYPE_UNSPECIFIED"
	FileTypeFile        FileType = "FILE_TYPE_FILE"
	FileTypeDirectory   FileType = "FILE_TYPE_DIRECTORY"
)

type statReq struct {
	Path string `json:"path"`
}
type statResp struct {
	Entry EntryInfo `json:"entry"`
}
type makeDirReq struct {
	Path string `json:"path"`
}
type moveReq struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}
type listDirReq struct {
	Path  string `json:"path"`
	Depth uint32 `json:"depth"`
}
type listDirResp struct {
	Entries []EntryInfo `json:"entries"`
}
type removeReq struct {
	Path string `json:"path"`
}
type entryResp struct {
	Entry EntryInfo `json:"entry"`
}

// ---- process message shapes ----

type ProcessConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Envs map[string]string `json:"envs,omitempty"`
	Cwd  *string           `json:"cwd,omitempty"`
}

// ProcResult is a run-to-completion result the streaming Start is built from.
type ProcResult struct {
	PID      uint32
	Stdout   []byte
	Stderr   []byte
	ExitCode int32
}

type startReq struct {
	Process ProcessConfig `json:"process"`
	Tag     *string       `json:"tag,omitempty"`
}
type listProcResp struct {
	Processes []any `json:"processes"`
}

// envd routes the connect-rpc + file-content HTTP endpoints.
type envd struct {
	backend EnvdBackend
}

func (e *envd) routes(mux *http.ServeMux) {
	// Filesystem service (unary).
	mux.HandleFunc("/filesystem.Filesystem/Stat", e.fsStat)
	mux.HandleFunc("/filesystem.Filesystem/MakeDir", e.fsMakeDir)
	mux.HandleFunc("/filesystem.Filesystem/Move", e.fsMove)
	mux.HandleFunc("/filesystem.Filesystem/ListDir", e.fsListDir)
	mux.HandleFunc("/filesystem.Filesystem/Remove", e.fsRemove)
	// Process service.
	mux.HandleFunc("/process.Process/List", e.procList)
	mux.HandleFunc("/process.Process/Start", e.procStart)
	// File content (plain HTTP): GET reads, POST writes; ?path=…
	mux.HandleFunc("/files", e.files)
}

// ---- filesystem unary handlers ----

func (e *envd) fsStat(w http.ResponseWriter, r *http.Request) {
	var req statReq
	if !decodeConnect(w, r, &req) {
		return
	}
	entry, err := e.backend.Stat(sandboxOf(r), req.Path)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	writeConnect(w, statResp{Entry: entry})
}

func (e *envd) fsMakeDir(w http.ResponseWriter, r *http.Request) {
	var req makeDirReq
	if !decodeConnect(w, r, &req) {
		return
	}
	entry, err := e.backend.MakeDir(sandboxOf(r), req.Path)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	writeConnect(w, entryResp{Entry: entry})
}

func (e *envd) fsMove(w http.ResponseWriter, r *http.Request) {
	var req moveReq
	if !decodeConnect(w, r, &req) {
		return
	}
	entry, err := e.backend.Move(sandboxOf(r), req.Source, req.Destination)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	writeConnect(w, entryResp{Entry: entry})
}

func (e *envd) fsListDir(w http.ResponseWriter, r *http.Request) {
	var req listDirReq
	if !decodeConnect(w, r, &req) {
		return
	}
	entries, err := e.backend.ListDir(sandboxOf(r), req.Path, req.Depth)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	if entries == nil {
		entries = []EntryInfo{}
	}
	writeConnect(w, listDirResp{Entries: entries})
}

func (e *envd) fsRemove(w http.ResponseWriter, r *http.Request) {
	var req removeReq
	if !decodeConnect(w, r, &req) {
		return
	}
	if err := e.backend.Remove(sandboxOf(r), req.Path); err != nil {
		writeConnectErr(w, err)
		return
	}
	writeConnect(w, struct{}{})
}

// ---- process handlers ----

func (e *envd) procList(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !decodeConnect(w, r, &req) {
		return
	}
	writeConnect(w, listProcResp{Processes: []any{}})
}

// procStart runs the command and emits a connect server-STREAM: a Start event
// (pid), a Data event per stdout/stderr chunk, then an End event (exit code).
func (e *envd) procStart(w http.ResponseWriter, r *http.Request) {
	var req startReq
	if !decodeConnect(w, r, &req) {
		return
	}
	res, err := e.backend.StartProcess(sandboxOf(r), req.Process)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)
	// StartEvent
	writeConnectFrame(w, map[string]any{"event": map[string]any{"start": map[string]any{"pid": res.PID}}})
	if len(res.Stdout) > 0 {
		writeConnectFrame(w, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": res.Stdout}}})
	}
	if len(res.Stderr) > 0 {
		writeConnectFrame(w, map[string]any{"event": map[string]any{"data": map[string]any{"stderr": res.Stderr}}})
	}
	writeConnectFrame(w, map[string]any{"event": map[string]any{"end": map[string]any{
		"exitCode": res.ExitCode, "exited": true, "status": "exited",
	}}})
	// connect end-of-stream frame (flag 0x02), empty error = success.
	writeConnectEndFrame(w)
}

// ---- file content HTTP ----

func (e *envd) files(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query param required", http.StatusBadRequest)
		return
	}
	sandboxID := sandboxOf(r)
	switch r.Method {
	case http.MethodGet:
		data, err := e.backend.ReadFile(sandboxID, path)
		if err != nil {
			writeConnectErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	case http.MethodPost, http.MethodPut:
		data, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 100<<20))
		entry, err := e.backend.WriteFile(sandboxID, path, data)
		if err != nil {
			writeConnectErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, entryResp{Entry: entry})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- connect helpers ----

func decodeConnect(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeConnectErrStatus(w, http.StatusMethodNotAllowed, "unimplemented", "POST required")
		return false
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if len(body) == 0 {
		return true // empty request message is valid (e.g. List)
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeConnectErrStatus(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return false
	}
	return true
}

func writeConnect(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// writeConnectErr maps a backend error to a connect error. A NotFoundError maps
// to connect "not_found"; anything else to "internal".
func writeConnectErr(w http.ResponseWriter, err error) {
	if _, ok := err.(NotFoundError); ok {
		writeConnectErrStatus(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeConnectErrStatus(w, http.StatusInternalServerError, "internal", err.Error())
}

func writeConnectErrStatus(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// writeConnectFrame writes one enveloped connect-stream JSON message: a 5-byte
// prefix (1 flag byte = 0, 4-byte big-endian length) followed by the JSON.
func writeConnectFrame(w http.ResponseWriter, v any) {
	payload, _ := json.Marshal(v)
	var hdr [5]byte
	hdr[0] = 0 // data frame
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	w.Write(hdr[:])
	w.Write(payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeConnectEndFrame writes the terminal end-of-stream frame (flag 0x02) with
// an empty body = success.
func writeConnectEndFrame(w http.ResponseWriter) {
	body := []byte("{}")
	var hdr [5]byte
	hdr[0] = 0x02 // end-of-stream
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(body)))
	w.Write(hdr[:])
	w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// NotFoundError lets a backend signal a connect not_found without importing
// this package's HTTP concerns.
type NotFoundError struct{ Msg string }

func (e NotFoundError) Error() string {
	if e.Msg == "" {
		return "not found"
	}
	return e.Msg
}
