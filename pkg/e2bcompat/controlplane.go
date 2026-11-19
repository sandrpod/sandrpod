// Copyright 2026 SandrPod Contributors
// E2B control-plane REST surface (api.<domain>). Handlers implement the exact
// paths/shapes from E2B's spec/openapi.yml, backed by a SandboxBackend that the
// SandrPod server satisfies over its scheduler + store.

package e2bcompat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrQuotaExceeded is returned by SandboxBackend.CreateSandbox when the caller
// is already at its per-owner sandbox limit. The control plane answers 429 for
// it, so a client can tell "you have too many" from "something broke".
var ErrQuotaExceeded = errors.New("sandbox quota exceeded")

// ErrAlreadyExists is returned when the requested sandbox name is already held
// by another record (possibly another tenant's). The native API answers 409 for
// the same condition; the E2B surface says it in its own vocabulary.
var ErrAlreadyExists = errors.New("sandbox already exists")

// SandboxBackend is what the control plane needs from SandrPod. The server
// implements it over its scheduler/store; tests use a fake. `identity` is the
// resolved API key (already authenticated) so the backend can attribute
// ownership/quota.
type SandboxBackend interface {
	// CreateSandbox provisions a sandbox and returns its E2B-facing view.
	// admin reports whether the caller is exempt from per-owner limits; return
	// ErrQuotaExceeded to have the refusal surfaced as 429 rather than 500.
	CreateSandbox(ident string, admin bool, req NewSandbox) (SandboxDetail, error)
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
	// GetMetrics returns resource metrics for a sandbox (empty slice if none).
	GetMetrics(ident, sandboxID string) ([]SandboxMetric, bool)
}

// SnapshotBackend is the optional E2B snapshot/template surface. A deployment
// that does not implement it answers 501 rather than pretending the call
// succeeded; the SDK surfaces that as a clear compatibility gap.
type SnapshotBackend interface {
	CreateSnapshot(ident, sandboxID, name string) (SnapshotInfo, error)
	ListSnapshots(ident, sandboxID string, limit int) []SnapshotInfo
	DeleteSnapshot(ident, snapshotID string) bool
}

// SnapshotInfo is E2B's snapshot template shape.
type SnapshotInfo struct {
	SnapshotID string   `json:"snapshotID"`
	Names      []string `json:"names"`
}

// NetworkUpdateBackend is the optional live egress-policy surface. A backend
// that cannot change a running sandbox's network returns an error; the gateway
// reports it as a conflict rather than pretending the update landed.
type NetworkUpdateBackend interface {
	UpdateNetwork(ident, sandboxID string, network *E2BNetwork, allowInternet *bool) error
}

// ConflictError is a request that is valid but cannot be applied to the
// current sandbox state (for example, removing an egress policy from a running
// sandbox). The control plane maps it to 409.
type ConflictError struct{ Msg string }

func (e ConflictError) Error() string { return e.Msg }

// ForkBackend is the optional E2B fork surface. A fork snapshots the source
// sandbox and starts `count` sandboxes from that image; per-item failures are
// returned in-band, as E2B requires.
type ForkBackend interface {
	ForkSandbox(ident, sandboxID string, timeout, count int32) ([]ForkResult, error)
}

// ForkResult is one element of E2B's fork response.
type ForkResult struct {
	Sandbox *Sandbox   `json:"sandbox,omitempty"`
	Error   *ForkError `json:"error,omitempty"`
}

// ForkError is E2B's per-fork error shape.
type ForkError struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
}

// SandboxMetric is one resource-usage sample (E2B SandboxMetric shape).
type SandboxMetric struct {
	CPUCount      int     `json:"cpuCount"`
	CPUUsedPct    float64 `json:"cpuUsedPct"`
	DiskTotal     uint64  `json:"diskTotal"`
	DiskUsed      uint64  `json:"diskUsed"`
	MemCache      uint64  `json:"memCache"`
	MemTotal      uint64  `json:"memTotal"`
	MemUsed       uint64  `json:"memUsed"`
	Timestamp     string  `json:"timestamp"`     // RFC3339
	TimestampUnix int64   `json:"timestampUnix"` // seconds
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
		mux.HandleFunc(prefix+"/snapshots", c.handleSnapshots)
		mux.HandleFunc(prefix+"/templates/", c.handleTemplateByID)
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
		// The tenant travels with the request so the backend can scope
		// placement, exactly as the native API does. It is not part of E2B's
		// wire format: the gateway fills it from the authenticated identity.
		req.Tenant = tenantOf(r)
		detail, err := c.backend.CreateSandbox(ident, adminOf(r), req)
		if err != nil {
			var invalid InvalidError
			if errors.As(err, &invalid) {
				writeErr(w, http.StatusBadRequest, invalid.Error())
				return
			}
			if errors.Is(err, ErrQuotaExceeded) {
				writeErr(w, http.StatusTooManyRequests, err.Error())
				return
			}
			if errors.Is(err, ErrAlreadyExists) {
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
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
	// The mux registers both /sandboxes/ and /v2/sandboxes/. Strip the version
	// prefix before parsing, otherwise /v2/sandboxes/<id>/connect is read as
	// sandbox "v2" with an unknown action and answers 405 (the current SDK's
	// Sandbox.connect uses the /v2 spelling).
	path := strings.TrimPrefix(r.URL.Path, "/v2")
	rest := strings.TrimPrefix(path, "/sandboxes/")
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
		// Sandbox.connect(id): attach to a sandbox, auto-resuming it if paused,
		// refreshing its timeout, and returning its info so the SDK can build the
		// envd client. Resume is idempotent for a running sandbox; it reports
		// unsupported for direct-agent sandboxes, where we fall back to a plain
		// get.
		var req ResumeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if detail, ok := c.backend.ResumeSandbox(ident, sandboxID, req.Timeout); ok {
			writeJSON(w, http.StatusOK, detail)
			return
		}
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

	case action == "network" && r.Method == http.MethodPut:
		ub, ok := c.backend.(NetworkUpdateBackend)
		if !ok {
			writeErr(w, http.StatusNotImplemented, "network updates are not supported by this deployment")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var network E2BNetwork
		if err := json.Unmarshal(raw, &network); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid network body: "+err.Error())
			return
		}
		var meta struct {
			AllowInternetAccess *bool `json:"allow_internet_access"`
		}
		_ = json.Unmarshal(raw, &meta)
		err = ub.UpdateNetwork(ident, sandboxID, &network, meta.AllowInternetAccess)
		if err != nil {
			var invalid InvalidError
			if errors.As(err, &invalid) {
				writeErr(w, http.StatusBadRequest, invalid.Error())
				return
			}
			var nf NotFoundError
			if errors.As(err, &nf) {
				writeErr(w, http.StatusNotFound, nf.Error())
				return
			}
			var conflict ConflictError
			if errors.As(err, &conflict) {
				writeErr(w, http.StatusConflict, conflict.Error())
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case action == "fork" && r.Method == http.MethodPost:
		fb, ok := c.backend.(ForkBackend)
		if !ok {
			writeErr(w, http.StatusNotImplemented, "fork is not supported by this deployment")
			return
		}
		var req struct {
			Timeout int32 `json:"timeout,omitempty"`
			Count   int32 `json:"count,omitempty"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
		if req.Count <= 0 {
			req.Count = 1
		}
		if req.Count > 10 {
			req.Count = 10
		}
		results, err := fb.ForkSandbox(ident, sandboxID, req.Timeout, req.Count)
		if err != nil {
			var nf NotFoundError
			if errors.As(err, &nf) {
				writeErr(w, http.StatusNotFound, nf.Error())
				return
			}
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, results)

	case action == "snapshots" && r.Method == http.MethodPost:
		sbk, ok := c.backend.(SnapshotBackend)
		if !ok {
			writeErr(w, http.StatusNotImplemented, "snapshots are not supported by this deployment")
			return
		}
		var req struct {
			Name string `json:"name,omitempty"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
		info, err := sbk.CreateSnapshot(ident, sandboxID, req.Name)
		if err != nil {
			var invalid InvalidError
			if errors.As(err, &invalid) {
				writeErr(w, http.StatusBadRequest, invalid.Error())
				return
			}
			var nf NotFoundError
			if errors.As(err, &nf) {
				writeErr(w, http.StatusNotFound, nf.Error())
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, info)

	case action == "metrics" && r.Method == http.MethodGet:
		metrics, ok := c.backend.GetMetrics(ident, sandboxID)
		if !ok {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if metrics == nil {
			metrics = []SandboxMetric{}
		}
		writeJSON(w, http.StatusOK, metrics)

	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleSnapshots is GET /snapshots (and /v2/snapshots): list templates.
func (c *controlPlane) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sbk, ok := c.backend.(SnapshotBackend)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "snapshots are not supported by this deployment")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	out := sbk.ListSnapshots(identOf(r), r.URL.Query().Get("sandboxID"), limit)
	if out == nil {
		out = []SnapshotInfo{}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTemplateByID is DELETE /templates/{snapshotID}; E2B uses a template ID
// for snapshot deletion.
func (c *controlPlane) handleTemplateByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sbk, ok := c.backend.(SnapshotBackend)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "snapshots are not supported by this deployment")
		return
	}
	raw := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v2"), "/templates/")
	id, err := url.PathUnescape(raw)
	if err != nil || id == "" {
		writeErr(w, http.StatusBadRequest, "snapshot id required")
		return
	}
	if !sbk.DeleteSnapshot(identOf(r), id) {
		writeErr(w, http.StatusNotFound, "snapshot not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SandboxFromDetail projects the full detail onto the lighter create/resume
// response schema. Exported for a host that builds a fork result itself.
func SandboxFromDetail(d SandboxDetail) Sandbox { return sandboxFromDetail(d) }

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
