// Copyright 2024 SandrPod
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
}

// CodeInterpreter runs code statefully in a sandbox context. The SandrPod
// server implements it by calling the toolbox /code-interpreter/execute.
type CodeInterpreter interface {
	RunCode(sandboxID, contextID, code string) (CodeExecution, error)
}

type codeInterp struct {
	backend CodeInterpreter
}

func (c *codeInterp) routes(mux *http.ServeMux) {
	mux.HandleFunc("/execute", c.execute)
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
	} else if ex.Text != "" {
		enc.Encode(map[string]any{"type": "result", "text": ex.Text, "is_main_result": true})
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
