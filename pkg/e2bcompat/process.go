// Copyright 2026 SandrPod Contributors
// E2B Process service: the pid-addressed process surface reached over connect-
// rpc at the envd host. It backs commands.run(background=…)/list/kill/send_stdin
// /connect and the PTY (pty.create/send_input/resize/kill), which the SDK layers
// entirely on this one service:
//
//	Start   (server-stream) — spawn; stream start→data→end events. pty+stdin flags.
//	Connect (server-stream) — re-attach to a pid and follow its output.
//	List    (unary)         — running processes.
//	SendInput (unary)       — write stdin (or PTY master).
//	SendSignal (unary)      — kill (SIGKILL/SIGTERM).
//	Update  (unary)         — resize a PTY.
//	CloseStdin (unary)      — close stdin.
//
// When the configured EnvdBackend also implements EnvdProcBackend, envd serves
// the full async surface by proxying to the toolbox /procmgr/* endpoints and
// transcoding the NDJSON event stream into connect frames. Otherwise it falls
// back to the buffered run-to-completion synth (StartProcess).

package e2bcompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// PTYSize is a pseudo-terminal window size.
type PTYSize struct {
	Rows uint32 `json:"rows"`
	Cols uint32 `json:"cols"`
}

// ProcInfo is the metadata List returns for a running process.
type ProcInfo struct {
	PID  uint32   `json:"pid"`
	Tag  string   `json:"tag,omitempty"`
	Cmd  string   `json:"cmd"`
	Args []string `json:"args,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
}

// EnvdProcBackend is the richer, pid-addressed process surface. StartProc and
// ConnectProc return an NDJSON stream of procEvent objects (start/stdout/stderr
// /pty/end); the caller transcodes them into connect frames.
type EnvdProcBackend interface {
	StartProc(sandboxID string, cfg ProcessConfig, pty *PTYSize, stdin bool) (io.ReadCloser, error)
	ConnectProc(sandboxID string, pid uint32) (io.ReadCloser, error)
	ListProcs(sandboxID string) ([]ProcInfo, error)
	SendProcInput(sandboxID string, pid uint32, data []byte, isPTY bool) error
	SignalProc(sandboxID string, pid uint32, signal int32) error
	ResizeProc(sandboxID string, pid uint32, rows, cols uint32) error
	CloseProcStdin(sandboxID string, pid uint32) error
}

// procEvent mirrors the toolbox procmgr event on the NDJSON stream.
type procEvent struct {
	Type     string `json:"type"` // start | stdout | stderr | pty | end
	PID      uint32 `json:"pid,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int32  `json:"exit_code"`
}

// procBackend returns the rich process backend if the configured EnvdBackend
// implements it.
func (e *envd) procBackend() (EnvdProcBackend, bool) {
	pb, ok := e.backend.(EnvdProcBackend)
	return pb, ok
}

// procStart handles the Start server-stream. With a rich backend it proxies to
// the async process table; otherwise it falls back to the buffered synth.
func (e *envd) procStart(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	proto := isProtoContentType(r.Header.Get("Content-Type"))
	msg := stripEnvelope(body)

	pb, ok := e.procBackend()
	if !ok {
		e.procStartBuffered(w, r, msg, proto)
		return
	}

	var (
		cfg   ProcessConfig
		pty   *PTYSize
		stdin bool
	)
	if proto {
		cfg, pty, stdin = decodeStartRequestFull(msg)
	} else {
		var req startReq
		_ = json.Unmarshal(msg, &req)
		cfg = req.Process
		stdin = req.Stdin
		if req.PTY != nil {
			pty = &PTYSize{Rows: req.PTY.Size.Rows, Cols: req.PTY.Size.Cols}
		}
	}
	rc, err := pb.StartProc(sandboxOf(r), cfg, pty, stdin)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	streamProcEvents(r.Context(), w, proto, rc)
}

// procConnect handles the Connect server-stream: re-attach to a pid.
func (e *envd) procConnect(w http.ResponseWriter, r *http.Request) {
	pb, ok := e.procBackend()
	if !ok {
		writeConnectErrStatus(w, http.StatusNotImplemented, "unimplemented", "connect not supported")
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	proto := isProtoContentType(r.Header.Get("Content-Type"))
	msg := stripEnvelope(body)
	var pid uint32
	if proto {
		pid = decodeConnectRequest(msg)
	} else {
		var req struct {
			Process struct {
				PID uint32 `json:"pid"`
			} `json:"process"`
		}
		_ = json.Unmarshal(msg, &req)
		pid = req.Process.PID
	}
	rc, err := pb.ConnectProc(sandboxOf(r), pid)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	streamProcEvents(r.Context(), w, proto, rc)
}

// procList handles the List unary.
func (e *envd) procList(w http.ResponseWriter, r *http.Request) {
	_, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	pb, hasRich := e.procBackend()
	var procs []ProcInfo
	if hasRich {
		var err error
		if procs, err = pb.ListProcs(sandboxOf(r)); err != nil {
			writeConnectErr(w, err)
			return
		}
	}
	if proto {
		writeUnary(w, proto, encodeListResponse(procs), nil)
		return
	}
	writeUnary(w, false, nil, listResponseJSON(procs))
}

// procSendInput handles the SendInput unary.
func (e *envd) procSendInput(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	pb, hasRich := e.procBackend()
	if !hasRich {
		writeConnectErrStatus(w, http.StatusNotImplemented, "unimplemented", "send_input not supported")
		return
	}
	var (
		pid   uint32
		data  []byte
		isPTY bool
	)
	if proto {
		pid, data, isPTY = decodeInputRequest(body)
	} else {
		var req struct {
			Process struct {
				PID uint32 `json:"pid"`
			} `json:"process"`
			Input struct {
				Stdin []byte `json:"stdin"`
				PTY   []byte `json:"pty"`
			} `json:"input"`
		}
		_ = json.Unmarshal(body, &req)
		pid = req.Process.PID
		if len(req.Input.PTY) > 0 {
			data, isPTY = req.Input.PTY, true
		} else {
			data = req.Input.Stdin
		}
	}
	if err := pb.SendProcInput(sandboxOf(r), pid, data, isPTY); err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, nil, struct{}{})
}

// procSendSignal handles the SendSignal unary (kill).
func (e *envd) procSendSignal(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	pb, hasRich := e.procBackend()
	if !hasRich {
		writeConnectErrStatus(w, http.StatusNotImplemented, "unimplemented", "send_signal not supported")
		return
	}
	var (
		pid    uint32
		signal int32
	)
	if proto {
		pid, signal = decodeSignalRequest(body)
	} else {
		var req struct {
			Process struct {
				PID uint32 `json:"pid"`
			} `json:"process"`
			Signal any `json:"signal"`
		}
		_ = json.Unmarshal(body, &req)
		pid = req.Process.PID
		signal = signalNumber(req.Signal)
	}
	if err := pb.SignalProc(sandboxOf(r), pid, signal); err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, nil, struct{}{})
}

// procCloseStdin handles the CloseStdin unary.
func (e *envd) procCloseStdin(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	pb, hasRich := e.procBackend()
	if !hasRich {
		writeConnectErrStatus(w, http.StatusNotImplemented, "unimplemented", "close_stdin not supported")
		return
	}
	var pid uint32
	if proto {
		pid = decodeSubMessagePID(body, 1)
	} else {
		var req struct {
			Process struct {
				PID uint32 `json:"pid"`
			} `json:"process"`
		}
		_ = json.Unmarshal(body, &req)
		pid = req.Process.PID
	}
	if err := pb.CloseProcStdin(sandboxOf(r), pid); err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, nil, struct{}{})
}

// procUpdate handles the Update unary (PTY resize).
func (e *envd) procUpdate(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	pb, hasRich := e.procBackend()
	if !hasRich {
		writeConnectErrStatus(w, http.StatusNotImplemented, "unimplemented", "update not supported")
		return
	}
	var pid, rows, cols uint32
	if proto {
		pid, rows, cols = decodeUpdateRequest(body)
	} else {
		var req struct {
			Process struct {
				PID uint32 `json:"pid"`
			} `json:"process"`
			PTY struct {
				Size PTYSize `json:"size"`
			} `json:"pty"`
		}
		_ = json.Unmarshal(body, &req)
		pid, rows, cols = req.Process.PID, req.PTY.Size.Rows, req.PTY.Size.Cols
	}
	if err := pb.ResizeProc(sandboxOf(r), pid, rows, cols); err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, nil, struct{}{})
}

// streamProcEvents transcodes a toolbox NDJSON procEvent stream into connect
// StartResponse/ConnectResponse frames. It closes rc when the client goes away
// (ctx cancelled) so the toolbox-side stream is torn down promptly.
func streamProcEvents(ctx context.Context, w http.ResponseWriter, proto bool, rc io.ReadCloser) {
	defer rc.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = rc.Close() // unblock the decoder
		case <-stop:
		}
	}()

	if proto {
		w.Header().Set("Content-Type", "application/connect+proto")
	} else {
		w.Header().Set("Content-Type", "application/connect+json")
	}
	w.WriteHeader(http.StatusOK)

	dec := json.NewDecoder(rc)
	for {
		var ev procEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		writeProcEventFrame(w, proto, ev)
	}
	writeConnectEndFrame(w)
}

// writeProcEventFrame writes one process event as a connect frame.
func writeProcEventFrame(w http.ResponseWriter, proto bool, ev procEvent) {
	if proto {
		switch ev.Type {
		case "start":
			writeEnvelope(w, 0, procStartEvent(ev.PID))
		case "stdout":
			writeEnvelope(w, 0, procDataEvent(ev.Data, chStdout))
		case "stderr":
			writeEnvelope(w, 0, procDataEvent(ev.Data, chStderr))
		case "pty":
			writeEnvelope(w, 0, procDataEvent(ev.Data, chPTY))
		case "end":
			writeEnvelope(w, 0, procEndEvent(ev.ExitCode, "exited"))
		}
		return
	}
	switch ev.Type {
	case "start":
		writeConnectFrame(w, map[string]any{"event": map[string]any{"start": map[string]any{"pid": ev.PID}}})
	case "stdout":
		writeConnectFrame(w, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": ev.Data}}})
	case "stderr":
		writeConnectFrame(w, map[string]any{"event": map[string]any{"data": map[string]any{"stderr": ev.Data}}})
	case "pty":
		writeConnectFrame(w, map[string]any{"event": map[string]any{"data": map[string]any{"pty": ev.Data}}})
	case "end":
		writeConnectFrame(w, map[string]any{"event": map[string]any{"end": map[string]any{
			"exitCode": ev.ExitCode, "exited": true, "status": "exited",
		}}})
	}
}

// listResponseJSON shapes ProcInfo into the proto3-JSON ListResponse.
func listResponseJSON(procs []ProcInfo) map[string]any {
	out := make([]map[string]any, 0, len(procs))
	for _, p := range procs {
		out = append(out, map[string]any{
			"pid": p.PID, "tag": p.Tag,
			"config": map[string]any{"cmd": p.Cmd, "args": p.Args, "cwd": p.Cwd},
		})
	}
	return map[string]any{"processes": out}
}

// signalNumber maps an E2B Signal (JSON: enum name string or number) to its
// numeric value. Only SIGKILL/SIGTERM are used by the SDK.
func signalNumber(v any) int32 {
	switch s := v.(type) {
	case string:
		switch s {
		case "SIGNAL_SIGTERM":
			return 15
		case "SIGNAL_SIGKILL":
			return 9
		}
	case float64:
		return int32(s)
	}
	return 9 // default SIGKILL
}
