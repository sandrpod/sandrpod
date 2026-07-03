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
	"mime"
	"net/http"
	"strings"
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
	Stdin   bool          `json:"stdin,omitempty"`
	PTY     *struct {
		Size PTYSize `json:"size"`
	} `json:"pty,omitempty"`
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
	mux.HandleFunc("/filesystem.Filesystem/CreateWatcher", e.fsCreateWatcher)
	mux.HandleFunc("/filesystem.Filesystem/GetWatcherEvents", e.fsGetWatcherEvents)
	mux.HandleFunc("/filesystem.Filesystem/RemoveWatcher", e.fsRemoveWatcher)
	// Process service.
	mux.HandleFunc("/process.Process/List", e.procList)
	mux.HandleFunc("/process.Process/Start", e.procStart)
	mux.HandleFunc("/process.Process/Connect", e.procConnect)
	mux.HandleFunc("/process.Process/SendInput", e.procSendInput)
	mux.HandleFunc("/process.Process/SendSignal", e.procSendSignal)
	mux.HandleFunc("/process.Process/Update", e.procUpdate)
	mux.HandleFunc("/process.Process/CloseStdin", e.procCloseStdin)
	// File content (plain HTTP): GET reads, POST writes; ?path=…
	mux.HandleFunc("/files", e.files)
}

// ---- filesystem unary handlers (JSON or binary-proto negotiated) ----

func (e *envd) fsStat(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	path := unaryPath(body, proto, func(b []byte) string { return decodeStringField(b, 1) }, func(v *statReq) string { return v.Path })
	entry, err := e.backend.Stat(sandboxOf(r), path)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, encodeStatResponse(entry), statResp{Entry: entry})
}

func (e *envd) fsMakeDir(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	path := unaryPath(body, proto, func(b []byte) string { return decodeStringField(b, 1) }, func(v *makeDirReq) string { return v.Path })
	entry, err := e.backend.MakeDir(sandboxOf(r), path)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, encodeEntryResponse(entry), entryResp{Entry: entry})
}

func (e *envd) fsMove(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	var src, dst string
	if proto {
		src, dst = decodeMoveRequest(body)
	} else {
		var req moveReq
		_ = json.Unmarshal(body, &req)
		src, dst = req.Source, req.Destination
	}
	entry, err := e.backend.Move(sandboxOf(r), src, dst)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, encodeEntryResponse(entry), entryResp{Entry: entry})
}

func (e *envd) fsListDir(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	var path string
	var depth uint32
	if proto {
		path, depth = decodeListDirRequest(body)
	} else {
		var req listDirReq
		_ = json.Unmarshal(body, &req)
		path, depth = req.Path, req.Depth
	}
	_ = depth
	entries, err := e.backend.ListDir(sandboxOf(r), path, depth)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	if entries == nil {
		entries = []EntryInfo{}
	}
	writeUnary(w, proto, encodeListDirResponse(entries), listDirResp{Entries: entries})
}

func (e *envd) fsRemove(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	path := unaryPath(body, proto, func(b []byte) string { return decodeStringField(b, 1) }, func(v *removeReq) string { return v.Path })
	if err := e.backend.Remove(sandboxOf(r), path); err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, nil, struct{}{})
}

// ---- process handlers ----
// The Start/Connect/List/SendInput/SendSignal/Update handlers live in
// process.go; they use the rich EnvdProcBackend when available. procStartBuffered
// below is the fallback for backends that only implement run-to-completion.

// procStartBuffered synthesizes a Start server-stream from a single buffered
// StartProcess result (start event, one data frame per stream, end event). Used
// when the backend does not implement EnvdProcBackend.
func (e *envd) procStartBuffered(w http.ResponseWriter, r *http.Request, msg []byte, proto bool) {
	var cfg ProcessConfig
	if proto {
		cfg = decodeStartRequest(msg)
	} else {
		var req startReq
		_ = json.Unmarshal(msg, &req)
		cfg = req.Process
	}
	res, err := e.backend.StartProcess(sandboxOf(r), cfg)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	if proto {
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		writeEnvelope(w, 0, procStartEvent(res.PID))
		if len(res.Stdout) > 0 {
			writeEnvelope(w, 0, procDataEvent(res.Stdout, chStdout))
		}
		if len(res.Stderr) > 0 {
			writeEnvelope(w, 0, procDataEvent(res.Stderr, chStderr))
		}
		writeEnvelope(w, 0, procEndEvent(res.ExitCode, "exited"))
		writeConnectEndFrame(w)
		return
	}
	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)
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
	writeConnectEndFrame(w)
}

// ---- unary negotiation helpers ----

// readUnary reads a unary connect request body and reports whether it is binary
// protobuf. Returns ok=false (and writes a 405) for non-POST.
func readUnary(w http.ResponseWriter, r *http.Request) ([]byte, bool, bool) {
	if r.Method != http.MethodPost {
		writeConnectErrStatus(w, http.StatusMethodNotAllowed, "unimplemented", "POST required")
		return nil, false, false
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	return body, isProtoContentType(r.Header.Get("Content-Type")), true
}

// unaryPath decodes a single path from either a proto message (via protoFn) or
// a JSON message (via jsonFn on a *T).
func unaryPath[T any](body []byte, proto bool, protoFn func([]byte) string, jsonFn func(*T) string) string {
	if proto {
		return protoFn(body)
	}
	var v T
	_ = json.Unmarshal(body, &v)
	return jsonFn(&v)
}

// writeUnary writes a unary connect response as binary proto or JSON.
func writeUnary(w http.ResponseWriter, proto bool, protoMsg []byte, jsonMsg any) {
	if proto {
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
		w.Write(protoMsg)
		return
	}
	writeConnect(w, jsonMsg)
}

// stripEnvelope removes a connect stream 5-byte frame prefix, returning the
// message bytes. If the body isn't enveloped it is returned unchanged.
func stripEnvelope(body []byte) []byte {
	if len(body) < 5 {
		return body
	}
	n := binary.BigEndian.Uint32(body[1:5])
	if int(n) <= len(body)-5 {
		return body[5 : 5+n]
	}
	return body[5:]
}

// writeEnvelope writes one connect stream frame (flag + big-endian length + msg).
func writeEnvelope(w http.ResponseWriter, flag byte, msg []byte) {
	var hdr [5]byte
	hdr[0] = flag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
	w.Write(hdr[:])
	w.Write(msg)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ---- file content HTTP ----

func (e *envd) files(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	sandboxID := sandboxOf(r)
	switch r.Method {
	case http.MethodGet:
		if path == "" {
			http.Error(w, "path query param required", http.StatusBadRequest)
			return
		}
		data, err := e.backend.ReadFile(sandboxID, path)
		if err != nil {
			writeConnectErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	case http.MethodPost, http.MethodPut:
		writes, perr := collectWrites(r, path)
		if perr != nil {
			writeConnectErrStatus(w, http.StatusBadRequest, "invalid_argument", perr.Error())
			return
		}
		out := make([]EntryInfo, 0, len(writes))
		for _, wr := range writes {
			entry, err := e.backend.WriteFile(sandboxID, wr.path, wr.data)
			if err != nil {
				writeConnectErr(w, err)
				return
			}
			out = append(out, entry)
		}
		// E2B's write returns an array of written-file entries (one per file;
		// a single write still yields a one-element array).
		writeJSON(w, http.StatusOK, out)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type fileWrite struct {
	path string
	data []byte
}

// rawFilename extracts the full (un-based) filename param from a
// Content-Disposition header, preserving any directory in the path.
func rawFilename(cd string) string {
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// collectWrites extracts one-or-more files to write from an envd file-write
// request. E2B uploads multipart/form-data where each "file" part's FILENAME is
// the destination path (batch write_files); a single write puts the path in the
// query too. A raw (octet-stream) body writes the query path.
func collectWrites(r *http.Request, queryPath string) ([]fileWrite, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		mr, err := r.MultipartReader()
		if err != nil {
			return nil, err
		}
		var out []fileWrite
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if part.FormName() != "file" {
				continue
			}
			data, err := io.ReadAll(part)
			if err != nil {
				return nil, err
			}
			// Each part's filename is the FULL destination path. Parse it from
			// Content-Disposition directly — part.FileName() runs filepath.Base
			// and would drop the directory (/tmp/a.txt → a.txt).
			p := rawFilename(part.Header.Get("Content-Disposition"))
			if p == "" {
				p = queryPath
			}
			out = append(out, fileWrite{path: p, data: data})
		}
		return out, nil
	}
	data, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 100<<20))
	if err != nil {
		return nil, err
	}
	return []fileWrite{{path: queryPath, data: data}}, nil
}

// ---- connect helpers ----

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
