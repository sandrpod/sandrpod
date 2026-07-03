// Copyright 2024 SandrPod
// E2B control-plane REST surface (api.<domain>). Handlers implement the exact
// paths/shapes from E2B's spec/openapi.yml, backed by a SandboxBackend that the
// SandrPod server satisfies over its scheduler + store.

package e2bcompat

import (
	"encoding/json"
	"net/http"
	"strings"
)

// SandboxBackend is what the control plane needs from SandrPod. The server
// implements it over its scheduler/store; tests use a fake. `identity` is the
// resolved API key (already authenticated) so the backend can attribute
// ownership/quota.
type SandboxBackend interface {
	// CreateSandbox provisions a sandbox and returns its E2B-facing view.
	CreateSandbox(ident string, req NewSandbox) (SandboxDetail, error)
	// GetSandbox returns one sandbox by E2B sandbox ID (nil,false if absent).
	GetSandbox(ident, sandboxID string) (SandboxDetail, bool)
	// ListSandboxes returns the caller's sandboxes, optionally filtered by
	// metadata (E2B passes a "key=value&key2=value2" style filter).
	ListSandboxes(ident string, metadataFilter map[string]string) []ListedSandbox
	// KillSandbox deletes a sandbox; returns false if it didn't exist.
	KillSandbox(ident, sandboxID string) bool
	// SetTimeout resets the idle timeout (seconds from now).
	SetTimeout(ident, sandboxID string, seconds int32) bool
	// Pause/Resume; if unsupported the backend may return false.
	PauseSandbox(ident, sandboxID string) bool
	ResumeSandbox(ident, sandboxID string, seconds int32) (SandboxDetail, bool)
}

// controlPlane serves the E2B REST API.
type controlPlane struct {
	backend SandboxBackend
}

func (c *controlPlane) routes(mux *http.ServeMux) {
	// The SDK uses both unversioned and /v2-prefixed control-plane paths.
	for _, prefix := range []string{"", "/v2"} {
		mux.HandleFunc(prefix+"/sandboxes", c.handleSandboxes)
		mux.HandleFunc(prefix+"/sandboxes/", c.handleSandboxByID)
	}
	// E2B health probe.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// handleSandboxes is /sandboxes: POST=create, GET=list.
func (c *controlPlane) handleSandboxes(w http.ResponseWriter, r *http.Request) {
	ident := identOf(r)
	switch r.Method {
	case http.MethodPost:
		var req NewSandbox
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && err.Error() != "EOF" {
			writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.Timeout == 0 {
			req.Timeout = DefaultTimeoutSeconds
		}
		detail, err := c.backend.CreateSandbox(ident, req)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Create returns the lighter Sandbox schema, not SandboxDetail.
		writeJSON(w, http.StatusCreated, sandboxFromDetail(detail))
	case http.MethodGet:
		filter := parseMetadataFilter(r.URL.Query().Get("metadata"))
		list := c.backend.ListSandboxes(ident, filter)
		if list == nil {
			list = []ListedSandbox{}
		}
		writeJSON(w, http.StatusOK, list)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleSandboxByID is /sandboxes/{id}[/timeout|/pause|/resume|/refreshes].
func (c *controlPlane) handleSandboxByID(w http.ResponseWriter, r *http.Request) {
	ident := identOf(r)
	rest := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
	if rest == "" {
		writeErr(w, http.StatusNotFound, "sandbox id required")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	sandboxID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		detail, ok := c.backend.GetSandbox(ident, sandboxID)
		if !ok {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeJSON(w, http.StatusOK, detail)

	case action == "" && r.Method == http.MethodDelete:
		if !c.backend.KillSandbox(ident, sandboxID) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case action == "timeout" && r.Method == http.MethodPost:
		var req SetTimeoutRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !c.backend.SetTimeout(ident, sandboxID, req.Timeout) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case action == "refreshes" && r.Method == http.MethodPost:
		var req RefreshRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		secs := req.Duration
		if secs <= 0 {
			secs = DefaultTimeoutSeconds
		}
		if !c.backend.SetTimeout(ident, sandboxID, secs) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case action == "connect" && r.Method == http.MethodPost:
		// Sandbox.connect(id): attach to a running sandbox, refreshing its
		// timeout, and return its info so the SDK can build the envd client.
		var req ResumeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Timeout > 0 {
			c.backend.SetTimeout(ident, sandboxID, req.Timeout)
		}
		detail, ok := c.backend.GetSandbox(ident, sandboxID)
		if !ok {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeJSON(w, http.StatusOK, detail)

	case action == "pause" && r.Method == http.MethodPost:
		if !c.backend.PauseSandbox(ident, sandboxID) {
			writeErr(w, http.StatusNotFound, "sandbox not found or pause unsupported")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case action == "resume" && r.Method == http.MethodPost:
		var req ResumeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		secs := req.Timeout
		if secs <= 0 {
			secs = DefaultTimeoutSeconds
		}
		detail, ok := c.backend.ResumeSandbox(ident, sandboxID, secs)
		if !ok {
			writeErr(w, http.StatusNotFound, "sandbox not found or resume unsupported")
			return
		}
		writeJSON(w, http.StatusCreated, sandboxFromDetail(detail))

	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// sandboxFromDetail projects the full detail onto the lighter create/resume
// response schema.
func sandboxFromDetail(d SandboxDetail) Sandbox {
	return Sandbox{
		TemplateID:      d.TemplateID,
		SandboxID:       d.SandboxID,
		Alias:           d.Alias,
		ClientID:        d.ClientID,
		EnvdVersion:     d.EnvdVersion,
		EnvdAccessToken: d.EnvdAccessToken,
		Domain:          d.Domain,
	}
}

// parseMetadataFilter parses E2B's "k1=v1&k2=v2" metadata filter.
func parseMetadataFilter(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, "&") {
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[k] = v
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Code: int32(status), Message: msg})
}
