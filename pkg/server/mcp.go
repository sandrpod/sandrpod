// Copyright 2026 SandrPod Contributors
// MCP (Model Context Protocol) surface: JSON-RPC 2.0 over HTTP, mounted at /mcp.
//
// This is the entry point agent clients (Claude Desktop, Cursor, Cline) use
// instead of writing an SDK. Every tool is a thin translation onto the REST API
// this same mux already serves — the tools dispatch a copied request back
// through the mux, so there is exactly one implementation of a create, an exec
// or a file read, and auth is re-checked by the target handler rather than
// reimplemented here.

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// jsonrpc errors
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpTool is one callable tool: a JSON schema plus the REST call it maps to.
type mcpTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// call builds the internal request. It returns method, path and body.
	call func(args map[string]any) (method, path string, body any, err error)
}

// captureWriter records what the mux handler wrote, so a tool can translate a
// REST response into MCP content without a second HTTP hop.
type captureWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newCaptureWriter() *captureWriter { return &captureWriter{header: http.Header{}} }

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) Write(p []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.body.Write(p)
}
func (c *captureWriter) WriteHeader(code int) { c.code = code }

// mcpHandler serves the protocol. `dispatch` runs a request through the mux.
type mcpHandler struct {
	dispatch func(*http.Request) *captureWriter
	tools    []mcpTool
	byName   map[string]mcpTool
}

// newMCPHandler builds the endpoint. dispatch is wired by the caller to the mux
// that serves the REST API.
func newMCPHandler(dispatch func(*http.Request) *captureWriter) http.Handler {
	h := &mcpHandler{dispatch: dispatch, byName: map[string]mcpTool{}}
	h.tools = builtinMCPTools()
	for _, t := range h.tools {
		h.byName[t.Name] = t
	}
	return h
}

func builtinMCPTools() []mcpTool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []mcpTool{
		{
			Name:        "sandbox_create",
			Description: "Create a sandbox. Returns once the record exists (state may still be STARTING); poll sandbox_list for RUNNING.",
			InputSchema: obj(map[string]any{
				"name":    str("unique sandbox name (letters, digits, dot, dash, underscore)"),
				"image":   str("optional image id (e.g. a desktop image); empty = the platform default"),
				"cpu":     num("cpu cores (default 1)"),
				"memory":  num("memory in MiB (default 1024)"),
				"ttl":     num("idle seconds before the sandbox is released (data persists)"),
				"node":    str("optional worker (poder) id to pin the sandbox to"),
				"region":  str("optional region"),
				"persist": map[string]any{"type": "boolean", "description": "keep /workspace across stop/start (default true)"},
			}, "name"),
			call: func(a map[string]any) (string, string, any, error) {
				body := map[string]any{
					"name":         strArg(a, "name"),
					"async":        true,
					"cpu_cores":    floatArg(a, "cpu", 1),
					"memory_mb":    floatArg(a, "memory", 1024),
					"ttl":          floatArg(a, "ttl", 3600),
					"image_id":     strArg(a, "image"),
					"poder_id":     strArg(a, "node"),
					"region":       strArg(a, "region"),
					"persist_data": boolArg(a, "persist", true),
				}
				if body["name"] == "" {
					return "", "", nil, fmt.Errorf("name is required")
				}
				return http.MethodPost, "/api/v1/sandboxes", body, nil
			},
		},
		{
			Name:        "sandbox_list",
			Description: "List sandboxes with their state, node and image.",
			InputSchema: obj(map[string]any{}),
			call: func(map[string]any) (string, string, any, error) {
				return http.MethodGet, "/api/v1/sandboxes", nil, nil
			},
		},
		{
			Name:        "sandbox_exec",
			Description: "Run a shell command inside the sandbox and return stdout, stderr and the exit code.",
			InputSchema: obj(map[string]any{
				"sandbox":  str("sandbox name"),
				"code":     str("command or script"),
				"language": str("bash (default) or another interpreter the image has"),
				"timeout":  num("seconds (default 60)"),
			}, "sandbox", "code"),
			call: func(a map[string]any) (string, string, any, error) {
				lang := strArg(a, "language")
				if lang == "" {
					lang = "bash"
				}
				return http.MethodPost, "/api/v1/sandboxes/execute?sandbox=" + urlq(strArg(a, "sandbox")),
					map[string]any{"code": strArg(a, "code"), "language": lang, "timeout": floatArg(a, "timeout", 60)}, nil
			},
		},
		{
			Name:        "sandbox_read_file",
			Description: "Read a file from the sandbox as text.",
			InputSchema: obj(map[string]any{
				"sandbox": str("sandbox name"),
				"path":    str("absolute path inside the sandbox"),
			}, "sandbox", "path"),
			call: func(a map[string]any) (string, string, any, error) {
				return http.MethodGet, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) +
					"/toolbox/files/download?path=" + urlq(strArg(a, "path")), nil, nil
			},
		},
		{
			Name:        "sandbox_write_file",
			Description: "Write text to a file in the sandbox, creating parent directories.",
			InputSchema: obj(map[string]any{
				"sandbox": str("sandbox name"),
				"path":    str("absolute path inside the sandbox"),
				"content": str("file contents"),
			}, "sandbox", "path", "content"),
			call: func(a map[string]any) (string, string, any, error) {
				return http.MethodPost, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) + "/toolbox/files/write",
					map[string]any{"path": strArg(a, "path"), "content": strArg(a, "content")}, nil
			},
		},
		{
			Name:        "sandbox_list_files",
			Description: "List a directory inside the sandbox.",
			InputSchema: obj(map[string]any{
				"sandbox": str("sandbox name"),
				"path":    str("directory (default /workspace)"),
			}, "sandbox"),
			call: func(a map[string]any) (string, string, any, error) {
				p := strArg(a, "path")
				if p == "" {
					p = "/workspace"
				}
				return http.MethodGet, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) +
					"/toolbox/files/list?path=" + urlq(p), nil, nil
			},
		},
		{
			Name:        "sandbox_start",
			Description: "Resume a stopped sandbox. Its /workspace data is still there.",
			InputSchema: obj(map[string]any{"sandbox": str("sandbox name")}, "sandbox"),
			call: func(a map[string]any) (string, string, any, error) {
				return http.MethodPost, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) + "/start", nil, nil
			},
		},
		{
			Name:        "sandbox_stop",
			Description: "Release the sandbox's CPU and memory without deleting it or its data.",
			InputSchema: obj(map[string]any{"sandbox": str("sandbox name")}, "sandbox"),
			call: func(a map[string]any) (string, string, any, error) {
				return http.MethodPost, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) + "/stop", nil, nil
			},
		},
		{
			Name:        "sandbox_delete",
			Description: "Delete the sandbox and its workspace data.",
			InputSchema: obj(map[string]any{"sandbox": str("sandbox name")}, "sandbox"),
			call: func(a map[string]any) (string, string, any, error) {
				return http.MethodDelete, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")), nil, nil
			},
		},
		{
			Name:        "sandbox_preview_url",
			Description: "Return the URL that exposes a port from inside the sandbox.",
			InputSchema: obj(map[string]any{
				"sandbox": str("sandbox name"),
				"port":    num("port the service listens on inside the sandbox"),
			}, "sandbox", "port"),
			call: func(a map[string]any) (string, string, any, error) {
				p := int(floatArg(a, "port", 0))
				if p <= 0 || p > 65535 {
					return "", "", nil, fmt.Errorf("port must be 1..65535")
				}
				return http.MethodGet, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) +
					"/toolbox/proxy/" + strconv.Itoa(p) + "/", nil, nil
			},
		},
	}
}

// ServeHTTP implements the MCP Streamable HTTP transport (JSON responses).
func (h *mcpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// A GET here is the optional SSE stream; this implementation answers
		// only request/response, so tell the client so instead of hanging.
		w.Header().Set("Allow", "POST")
		http.Error(w, "MCP endpoint accepts POST (JSON-RPC)", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeRPC(w, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: rpcParseError, Message: "cannot read body"}})
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeRPC(w, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: rpcParseError, Message: "invalid JSON"}})
		return
	}
	// Notifications (no id) get no response body.
	notification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		if notification {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "sandrpod", "version": "1.0.0"},
		}})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
	case "tools/list":
		tools := make([]map[string]any, 0, len(h.tools))
		for _, t := range h.tools {
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": tools}})
	case "tools/call":
		h.callTool(w, r, req)
	default:
		writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &mcpError{Code: rpcMethodNotFound, Message: "unknown method " + req.Method}})
	}
}

func (h *mcpHandler) callTool(w http.ResponseWriter, r *http.Request, req mcpRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: rpcInvalidParams, Message: "invalid params"}})
		return
	}
	tool, ok := h.byName[params.Name]
	if !ok {
		writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &mcpError{Code: rpcMethodNotFound, Message: "unknown tool " + params.Name}})
		return
	}
	method, path, body, err := tool.call(params.Arguments)
	if err != nil {
		writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: rpcInvalidParams, Message: err.Error()}})
		return
	}

	// Dispatch through the mux with the caller's credentials so the target
	// handler authenticates and authorizes exactly as it would for REST.
	inner := r.Clone(r.Context())
	inner.Method = method
	inner.URL.Path = path
	inner.URL.RawQuery = ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		inner.URL.Path = path[:i]
		inner.URL.RawQuery = path[i+1:]
	}
	inner.RequestURI = path
	if body != nil {
		encoded, _ := json.Marshal(body)
		inner.Body = io.NopCloser(bytes.NewReader(encoded))
		inner.ContentLength = int64(len(encoded))
		inner.Header.Set("Content-Type", "application/json")
	} else {
		inner.Body = http.NoBody
		inner.ContentLength = 0
	}
	resp := h.dispatch(inner)
	text := resp.body.String()
	if resp.code >= 400 {
		writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("HTTP %d: %s", resp.code, strings.TrimSpace(text))}},
			"isError": true,
		}})
		return
	}
	writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}})
}

func writeRPC(w http.ResponseWriter, resp mcpResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func urlq(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), " ", "%20") }

func strArg(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func floatArg(a map[string]any, k string, def float64) float64 {
	switch v := a[k].(type) {
	case float64:
		return v
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func boolArg(a map[string]any, k string, def bool) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return def
}
