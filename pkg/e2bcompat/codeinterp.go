// Copyright 2026 SandrPod Contributors
// E2B code-interpreter surface. The @e2b/code-interpreter SDK's run_code()
// connects to a code-interpreter server at <ciPort>-<sandboxID>.<domain> and
// POSTs to /execute, consuming an NDJSON stream of output messages that it
// aggregates into an Execution (logs.stdout/stderr, results[].text, error).

package e2bcompat

import (
	"encoding/json"
	"net/http"
	"strings"
)

// DefaultCodeInterpreterPort is the port the E2B code-interpreter SDK targets.
const DefaultCodeInterpreterPort = 49999

// CodeExecution is one stateful run_code outcome.
type CodeExecution struct {
	Stdout string
	Stderr string
	Text   string // value of the final expression (main result)
	Error  string // traceback if the cell raised
	// Images are base64-encoded PNGs (fallback when Results is unset).
	Images []string
	// Results is the E2B-shaped rich result list; each map carries a result's
	// MIME reprs (text/html/svg/png/latex/…) and is_main_result. Emitted verbatim
	// as E2B result messages so the SDK's Execution.results matches official E2B.
	Results []map[string]any
}

// CodeContext is an E2B code-interpreter context ({id, language, cwd}).
type CodeContext struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Cwd      string `json:"cwd"`
}

// CodeInterpreter runs code statefully in a sandbox context. The SandrPod
// server implements it by calling the toolbox /code-interpreter/*.
type CodeInterpreter interface {
	RunCode(sandboxID, contextID, code string) (CodeExecution, error)
	CreateContext(sandboxID, language, cwd string) (CodeContext, error)
	ListContexts(sandboxID string) ([]CodeContext, error)
	RemoveContext(sandboxID, contextID string) error
	RestartContext(sandboxID, contextID string) error
}

type codeInterp struct {
	backend CodeInterpreter
}

func (c *codeInterp) routes(mux *http.ServeMux) {
	mux.HandleFunc("/execute", c.execute)
	mux.HandleFunc("/contexts", c.contexts)
	mux.HandleFunc("/contexts/", c.contextByID)
}

// contexts: POST creates, GET lists.
func (c *codeInterp) contexts(w http.ResponseWriter, r *http.Request) {
	sid := sandboxOf(r)
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Language string `json:"language"`
			Cwd      string `json:"cwd"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ctx, err := c.backend.CreateContext(sid, req.Language, req.Cwd)
		if err != nil {
			writeConnectErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ctx)
	case http.MethodGet:
		list, err := c.backend.ListContexts(sid)
		if err != nil {
			writeConnectErr(w, err)
			return
		}
		if list == nil {
			list = []CodeContext{}
		}
		writeJSON(w, http.StatusOK, list)
	default:
		writeConnectErrStatus(w, http.StatusMethodNotAllowed, "unimplemented", "method not allowed")
	}
}

// contextByID: DELETE /contexts/{id} removes; POST /contexts/{id}/restart restarts.
func (c *codeInterp) contextByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/contexts/")
	id, action, _ := strings.Cut(rest, "/")
	sid := sandboxOf(r)
	switch {
	case action == "restart" && r.Method == http.MethodPost:
		if err := c.backend.RestartContext(sid, id); err != nil {
			writeConnectErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case action == "" && r.Method == http.MethodDelete:
		if err := c.backend.RemoveContext(sid, id); err != nil {
			writeConnectErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeConnectErrStatus(w, http.StatusMethodNotAllowed, "unimplemented", "method not allowed")
	}
}

// execute POSTs {code, context_id?, language?} and streams E2B NDJSON output
// messages: stdout/stderr/result/error then end_of_execution.
func (c *codeInterp) execute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeConnectErrStatus(w, http.StatusMethodNotAllowed, "unimplemented", "POST required")
		return
	}
	var req struct {
		Code      string `json:"code"`
		ContextID string `json:"context_id"`
		Language  string `json:"language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConnectErrStatus(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	if req.ContextID == "" {
		req.ContextID = "default"
	}
	ex, err := c.backend.RunCode(sandboxOf(r), req.ContextID, req.Code)
	if err != nil {
		writeConnectErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	if ex.Stdout != "" {
		enc.Encode(map[string]any{"type": "stdout", "text": ex.Stdout})
		flush()
	}
	if ex.Stderr != "" {
		enc.Encode(map[string]any{"type": "stderr", "text": ex.Stderr})
		flush()
	}
	if ex.Error != "" {
		enc.Encode(map[string]any{
			"type": "error", "name": "Error",
			"value": firstLine(ex.Error), "traceback": ex.Error,
		})
	} else if len(ex.Results) > 0 {
		// Emit each rich result verbatim (text/html/svg/png/latex/…) so the E2B
		// SDK's Execution.results matches what official E2B returns.
		for _, res := range ex.Results {
			msg := map[string]any{"type": "result"}
			for k, v := range res {
				msg[k] = v
			}
			enc.Encode(msg)
			flush()
		}
	} else {
		// Fallback for backends that only expose text/images.
		for _, img := range ex.Images {
			enc.Encode(map[string]any{"type": "result", "png": img, "is_main_result": false})
			flush()
		}
		if ex.Text != "" {
			enc.Encode(map[string]any{"type": "result", "text": ex.Text, "is_main_result": true})
		}
	}
	enc.Encode(map[string]any{"type": "end_of_execution"})
	flush()
}

func firstLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:] // last line of a traceback is the exception summary
	}
	return s
}
