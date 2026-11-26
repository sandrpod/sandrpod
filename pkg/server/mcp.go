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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
// mcpRawBody carries a pre-encoded request body (e.g. a multipart upload) through
// the internal dispatch, which otherwise always serialises as JSON.
type mcpRawBody struct {
	contentType string
	data        []byte
}

type mcpTool struct {
	// custom handles tools that talk to a sandbox's own MCP bridge (JSON-RPC)
	// rather than translating onto a REST endpoint.
	custom func(h *mcpHandler, w http.ResponseWriter, r *http.Request, req mcpRequest, args map[string]any)
	// render turns the dispatched endpoint's JSON into MCP content blocks. Without
	// it the body is one text block — right for a JSON answer, wrong for a chart: a
	// base64 PNG belongs in an image block so the client draws it.
	render      func(body []byte) []map[string]any
	Name        string
	Description string
	InputSchema map[string]any
	// call builds the internal request. It returns method, path and body.
	call func(args map[string]any) (method, path string, body any, err error)
}

type mcpSessionKey struct {
	callerKey string
	sandbox   string
}

type mcpCachedSession struct {
	id        string
	expiresAt time.Time
}

const mcpSessionCacheTTL = 5 * time.Minute
const mcpSessionCacheMax = 1024

// mcpHandler serves the protocol. `dispatch` runs a request through the mux.
type mcpHandler struct {
	dispatch func(*http.Request) *captureWriter
	tools    []mcpTool
	byName   map[string]mcpTool

	sessionMu sync.Mutex
	sessions  map[mcpSessionKey]mcpCachedSession
}

// newMCPHandler builds the endpoint. dispatch is wired by the caller to the mux
// that serves the REST API.
func newMCPHandler(dispatch func(*http.Request) *captureWriter) http.Handler {
	h := &mcpHandler{
		dispatch: dispatch,
		byName:   map[string]mcpTool{},
		sessions: map[mcpSessionKey]mcpCachedSession{},
	}
	h.tools = builtinMCPTools()
	for _, t := range h.tools {
		h.byName[t.Name] = t
	}
	return h
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

func builtinMCPTools() []mcpTool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	tools := []mcpTool{
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
				"desktop": map[string]any{"type": "boolean", "description": "this image runs a desktop on port 6901 (so the screen can be watched/driven)"},
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
					"desktop":      boolArg(a, "desktop", false),
					"gpu":          int(floatArg(a, "gpu", 0)),
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
				path := strArg(a, "path")
				var buf bytes.Buffer
				w := multipart.NewWriter(&buf)
				part, err := w.CreateFormFile("file", filepath.Base(path))
				if err != nil {
					return "", "", nil, err
				}
				if _, err := part.Write([]byte(strArg(a, "content"))); err != nil {
					return "", "", nil, err
				}
				if err := w.Close(); err != nil {
					return "", "", nil, err
				}
				return http.MethodPost, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) +
						"/toolbox/files/upload?path=" + urlq(path),
					&mcpRawBody{contentType: w.FormDataContentType(), data: buf.Bytes()}, nil
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
					"/toolbox/files?path=" + urlq(p), nil, nil
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
			Name:        "sandbox_snapshot",
			Description: "Commit the sandbox's filesystem to an image. Create a new sandbox with sandbox_create(image=<this image>) to fork it.",
			InputSchema: obj(map[string]any{
				"sandbox": str("sandbox name"),
				"image":   str("image reference to write, e.g. sandrpod-snapshot/box:latest"),
			}, "sandbox"),
			call: func(a map[string]any) (string, string, any, error) {
				path := "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) + "/snapshot"
				if img := strArg(a, "image"); img != "" {
					path += "?image=" + urlq(img)
				}
				return http.MethodPost, path, nil, nil
			},
		},
		{
			Name:        "sandbox_preview_url",
			Description: "Return the signed URL that exposes a port from inside the sandbox (open it as-is: the token is in the query).",
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
					"/preview/" + strconv.Itoa(p), nil, nil
			},
		},
	}

	// Generic bridge into any sandbox's own MCP server (desktop tools, headless
	// browser tools, and user-configured servers from mcp.json). One static
	// platform endpoint can therefore discover and invoke any tool inside any
	// sandbox without bloating tools/list or hardcoding per-image tool schemas.
	tools = append(tools,
		mcpTool{
			Name:        "sandbox_mcp_list",
			Description: "List MCP tools exposed inside a sandbox (desktop tools, headless browser tools, and servers in the sandbox's mcp.json). Call this first to discover tool names and argument schemas for sandbox_mcp_call.",
			InputSchema: obj(map[string]any{
				"sandbox":   str("sandbox name"),
				"mcp_token": str("optional per-sandbox MCP bearer token (only needed when the sandbox/worker enforces -mcp-token)"),
			}, "sandbox"),
			custom: (*mcpHandler).handleSandboxMCPList,
		},
		mcpTool{
			Name:        "sandbox_mcp_call",
			Description: "Call an MCP tool exposed inside a sandbox (discovered via sandbox_mcp_list). Returns the sandbox tool's content blocks directly, including images.",
			InputSchema: obj(map[string]any{
				"sandbox":   str("sandbox name"),
				"tool":      str("tool name inside the sandbox (from sandbox_mcp_list)"),
				"arguments": map[string]any{"type": "object", "description": "arguments object passed to the sandbox tool"},
				"mcp_token": str("optional per-sandbox MCP bearer token (only needed when the sandbox/worker enforces -mcp-token)"),
			}, "sandbox", "tool"),
			custom: (*mcpHandler).handleSandboxMCPCall,
		},
	)
	// The code interpreter: a stateful kernel, and charts come back as images. This
	// is the data-analysis surface — an agent computes in one cell, plots in the
	// next, and its context survives between calls.
	tools = append(tools,
		mcpTool{
			Name:        "sandbox_run_code",
			Description: "Run Python in a stateful kernel and return stdout plus any figures as images. The same context (default: the sandbox's) keeps variables between calls.",
			InputSchema: obj(map[string]any{
				"sandbox": str("sandbox name"),
				"code":    str("code to run"),
				"context": str("optional kernel id; omit for the sandbox's default one"),
				"timeout": num("seconds (default 120)"),
			}, "sandbox", "code"),
			call: func(a map[string]any) (string, string, any, error) {
				body := map[string]any{"code": strArg(a, "code")}
				if ctx := strArg(a, "context"); ctx != "" {
					body["context_id"] = ctx
				}
				if t := floatArg(a, "timeout", 0); t > 0 {
					body["timeout"] = t
				}
				return http.MethodPost, "/api/v1/sandboxes/" + urlq(strArg(a, "sandbox")) +
					"/toolbox/code-interpreter/execute", body, nil
			},
			render: renderCodeResult,
		},
		mcpTool{
			Name:        "sandbox_contexts",
			Description: "List, create, restart or remove a sandbox's code-interpreter kernels (a kernel is a namespace: variables, imports, loaded data).",
			InputSchema: obj(map[string]any{
				"sandbox": str("sandbox name"),
				"action":  str("list (default) | create | restart | remove"),
				"context": str("kernel id (required for restart/remove)"),
			}, "sandbox"),
			call: func(a map[string]any) (string, string, any, error) {
				sb := urlq(strArg(a, "sandbox"))
				action := strArg(a, "action")
				if action == "" {
					action = "list"
				}
				id := urlq(strArg(a, "context"))
				switch action {
				case "list":
					return http.MethodGet, "/api/v1/sandboxes/" + sb + "/toolbox/code-interpreter/contexts", nil, nil
				case "create":
					return http.MethodPost, "/api/v1/sandboxes/" + sb + "/toolbox/code-interpreter/contexts",
						map[string]any{"language": "python"}, nil
				case "restart":
					if id == "" {
						return "", "", nil, fmt.Errorf("restart needs the kernel id as `context`")
					}
					return http.MethodPost, "/api/v1/sandboxes/" + sb + "/toolbox/code-interpreter/contexts/" + id + "/restart", nil, nil
				case "remove":
					if id == "" {
						return "", "", nil, fmt.Errorf("remove needs the kernel id as `context`")
					}
					return http.MethodDelete, "/api/v1/sandboxes/" + sb + "/toolbox/code-interpreter/contexts/" + id, nil, nil
				}
				return "", "", nil, fmt.Errorf("action must be list, create, restart or remove")
			},
		},
	)
	return tools
}

// renderCodeResult turns the toolbox's CodeResult into MCP content: the text the
// user asked for, and every PNG as an image block so a chart arrives as a chart.
func renderCodeResult(body []byte) []map[string]any {
	var r struct {
		Stdout string   `json:"stdout"`
		Stderr string   `json:"stderr"`
		Text   string   `json:"text"`
		Error  string   `json:"error"`
		Images []string `json:"images"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return []map[string]any{{"type": "text", "text": string(body)}}
	}
	var out []map[string]any
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			out = append(out, map[string]any{"type": "text", "text": strings.TrimRight(s, "\n")})
		}
	}
	add(r.Stdout)
	add(r.Text)
	add(r.Stderr)
	if r.Error != "" {
		add("Traceback:\n" + r.Error)
	}
	for _, b64 := range r.Images {
		if b64 != "" {
			out = append(out, map[string]any{"type": "image", "data": b64, "mimeType": "image/png"})
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "text", "text": "(no output)"})
	}
	return out
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
	if tool.custom != nil {
		tool.custom(h, w, r, req, params.Arguments)
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
	if raw, ok := body.(*mcpRawBody); ok {
		inner.Body = io.NopCloser(bytes.NewReader(raw.data))
		inner.ContentLength = int64(len(raw.data))
		inner.Header.Set("Content-Type", raw.contentType)
	} else if body != nil {
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
	content := []map[string]any{{"type": "text", "text": text}}
	if tool.render != nil {
		if shaped := tool.render(resp.body.Bytes()); len(shaped) > 0 {
			content = shaped
		}
	}
	writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": content,
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

// handleSandboxMCPList forwards tools/list to the target sandbox's MCP bridge
// and returns the tool definitions as JSON text.
func (h *mcpHandler) handleSandboxMCPList(w http.ResponseWriter, r *http.Request, req mcpRequest, args map[string]any) {
	sandbox := strArg(args, "sandbox")
	if sandbox == "" {
		writeToolError(w, req, "sandbox is required")
		return
	}
	mcpToken := strArg(args, "mcp_token")

	result, errMsg := h.forwardSandboxMCP(r, sandbox, mcpToken, "tools/list", nil)
	if errMsg != "" {
		writeToolError(w, req, errMsg)
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		writeToolError(w, req, "failed to encode sandbox tools: "+err.Error())
		return
	}
	writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(encoded)}},
		"isError": false,
	}})
}

// handleSandboxMCPCall forwards tools/call to the target sandbox's MCP bridge
// and passes its MCP result straight through so image blocks and isError stay intact.
func (h *mcpHandler) handleSandboxMCPCall(w http.ResponseWriter, r *http.Request, req mcpRequest, args map[string]any) {
	sandbox := strArg(args, "sandbox")
	if sandbox == "" {
		writeToolError(w, req, "sandbox is required")
		return
	}
	toolName := strArg(args, "tool")
	if toolName == "" {
		writeToolError(w, req, "tool is required (call sandbox_mcp_list first to discover available tools)")
		return
	}
	mcpToken := strArg(args, "mcp_token")

	var toolArgs map[string]any
	if raw, ok := args["arguments"]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			writeToolError(w, req, "arguments must be a JSON object")
			return
		}
		toolArgs = m
	} else {
		toolArgs = map[string]any{}
	}

	result, errMsg := h.forwardSandboxMCP(r, sandbox, mcpToken, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": toolArgs,
	})
	if errMsg != "" {
		writeToolError(w, req, errMsg)
		return
	}
	writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func writeToolError(w http.ResponseWriter, req mcpRequest, msg string) {
	writeRPC(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}})
}

// mcpCallerKey hashes the caller's credentials so cached Mcp-Session-Ids are
// strictly scoped per tenant/credential without storing raw secrets in memory.
func mcpCallerKey(r *http.Request, mcpToken string) string {
	sum := sha256.New()
	_, _ = sum.Write([]byte(r.Header.Get("X-Sandrpod-Token")))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write([]byte(r.Header.Get("Authorization")))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write([]byte(r.Header.Get("Cookie")))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write([]byte(mcpToken))
	return hex.EncodeToString(sum.Sum(nil))
}

func (h *mcpHandler) getCachedSession(key mcpSessionKey) string {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	entry, ok := h.sessions[key]
	if !ok {
		return ""
	}
	if time.Now().After(entry.expiresAt) {
		delete(h.sessions, key)
		return ""
	}
	return entry.id
}

func (h *mcpHandler) putCachedSession(key mcpSessionKey, sessionID string) {
	if sessionID == "" {
		return
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	now := time.Now()
	if len(h.sessions) >= mcpSessionCacheMax {
		for k, v := range h.sessions {
			if now.After(v.expiresAt) {
				delete(h.sessions, k)
			}
		}
		if len(h.sessions) >= mcpSessionCacheMax {
			h.sessions = map[mcpSessionKey]mcpCachedSession{}
		}
	}
	h.sessions[key] = mcpCachedSession{id: sessionID, expiresAt: now.Add(mcpSessionCacheTTL)}
}

func (h *mcpHandler) deleteCachedSession(key mcpSessionKey) {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	delete(h.sessions, key)
}

// forwardSandboxMCP sends one JSON-RPC method to /api/v1/sandboxes/{sandbox}/mcp
// through the server mux (so auth, tenant scoping, and both poder and direct
// agent routing apply unchanged). Reuses a cached Mcp-Session-Id per
// (caller, sandbox) and transparently re-initializes once if the session expired.
func (h *mcpHandler) forwardSandboxMCP(r *http.Request, sandbox, mcpToken, method string, params map[string]any) (map[string]any, string) {
	path := "/api/v1/sandboxes/" + urlq(sandbox) + "/mcp"

	post := func(payload map[string]any, session string) *captureWriter {
		encoded, _ := json.Marshal(payload)
		inner := r.Clone(r.Context())
		inner.Method = http.MethodPost
		inner.URL.Path = path
		inner.URL.RawQuery = ""
		inner.RequestURI = path
		inner.Body = io.NopCloser(bytes.NewReader(encoded))
		inner.ContentLength = int64(len(encoded))
		inner.Header.Set("Content-Type", "application/json")
		inner.Header.Set("Accept", "application/json")

		// Split platform auth vs per-sandbox MCP bearer so the platform API key
		// is never forwarded into the worker/sandbox: move a platform Bearer
		// token into X-Sandrpod-Token (which copyProxyHeaders strips) and only
		// put an explicit mcp_token (or a caller's separate Authorization when
		// X-Sandrpod-Token was already set) in Authorization.
		if inner.Header.Get("X-Sandrpod-Token") == "" {
			if auth := inner.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
				inner.Header.Set("X-Sandrpod-Token", strings.TrimSpace(auth[len("bearer "):]))
				inner.Header.Del("Authorization")
			}
		}
		if mcpToken != "" {
			inner.Header.Set("Authorization", "Bearer "+mcpToken)
		}

		if session != "" {
			inner.Header.Set("Mcp-Session-Id", session)
		} else {
			inner.Header.Del("Mcp-Session-Id")
		}
		return h.dispatch(inner)
	}

	initSession := func() (string, string) {
		initResp := post(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "sandrpod-platform", "version": "1"},
			},
		}, "")
		if initResp.code >= 400 {
			return "", formatForwardHTTPError(initResp)
		}
		return initResp.header.Get("Mcp-Session-Id"), ""
	}

	cacheKey := mcpSessionKey{callerKey: mcpCallerKey(r, mcpToken), sandbox: sandbox}
	session := h.getCachedSession(cacheKey)
	usedCached := session != ""
	if !usedCached {
		var errMsg string
		session, errMsg = initSession()
		if errMsg != "" {
			return nil, errMsg
		}
		h.putCachedSession(cacheKey, session)
	}

	callPayload := map[string]any{"jsonrpc": "2.0", "id": 2, "method": method}
	if params != nil {
		callPayload["params"] = params
	}

	resp := post(callPayload, session)
	if usedCached && isStaleMCPSession(resp) {
		h.deleteCachedSession(cacheKey)
		var errMsg string
		session, errMsg = initSession()
		if errMsg != "" {
			return nil, errMsg
		}
		h.putCachedSession(cacheKey, session)
		resp = post(callPayload, session)
	}

	if resp.code >= 400 {
		if usedCached {
			h.deleteCachedSession(cacheKey)
		}
		return nil, formatForwardHTTPError(resp)
	}

	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *mcpError      `json:"error"`
	}
	if err := json.Unmarshal(resp.body.Bytes(), &envelope); err != nil {
		return nil, "unreadable answer from the sandbox: " + err.Error()
	}
	if envelope.Error != nil {
		return nil, envelope.Error.Message
	}
	return envelope.Result, ""
}

func isStaleMCPSession(resp *captureWriter) bool {
	if resp.code == http.StatusBadRequest || resp.code == http.StatusNotFound {
		body := strings.ToLower(resp.body.String())
		if strings.Contains(body, "session") {
			return true
		}
	}
	var env struct {
		Error *mcpError `json:"error"`
	}
	if json.Unmarshal(resp.body.Bytes(), &env) == nil && env.Error != nil {
		if strings.Contains(strings.ToLower(env.Error.Message), "session") {
			return true
		}
	}
	return false
}

func formatForwardHTTPError(resp *captureWriter) string {
	text := strings.TrimSpace(resp.body.String())
	// A 404 here is usually the tenant check answering — say so in words the
	// client can act on instead of echoing an HTTP status.
	if resp.code == http.StatusNotFound {
		return "no such sandbox (or it does not belong to this credential)"
	}
	if text == "" {
		return fmt.Sprintf("HTTP %d from sandbox MCP", resp.code)
	}
	return fmt.Sprintf("HTTP %d: %s", resp.code, text)
}
