// Copyright 2024 SandrPod
// Package e2bcompat implements a wire-protocol-compatible gateway for the E2B
// SDKs (e2b / e2b-code-interpreter). The goal is a zero-config drop-in: point
// the unmodified E2B SDK at a SandrPod deployment via E2B_DOMAIN + an
// e2b_<hex> API key and it just works.
//
// E2B has two planes (see docs/E2B_COMPAT.md):
//   - control plane at https://api.<domain>  — REST/OpenAPI, X-API-KEY auth
//   - per-sandbox envd at https://<port>-<sandboxID>.<domain> — connect-rpc
//
// This package terminates BOTH at the SandrPod server: control-plane handlers
// map onto a SandboxBackend (our scheduler/store) and the envd connect-rpc
// services map onto an EnvdBackend (our toolbox, reached over the tunnel).
package e2bcompat

import "time"

// The schemas below mirror E2B's spec/openapi.yml exactly (field names and JSON
// tags) so the SDK's generated client deserializes our responses unchanged.

// NewSandbox is the POST /sandboxes request body.
type NewSandbox struct {
	TemplateID          string            `json:"templateID"`
	Timeout             int32             `json:"timeout,omitempty"` // seconds; E2B default 15
	AutoPause           bool              `json:"autoPause,omitempty"`
	AutoPauseMemory     *bool             `json:"autoPauseMemory,omitempty"`
	Secure              bool              `json:"secure,omitempty"`
	AllowInternetAccess *bool             `json:"allow_internet_access,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	EnvVars             map[string]string `json:"envVars,omitempty"`
}

// Sandbox is the POST /sandboxes (and resume) response.
type Sandbox struct {
	TemplateID         string  `json:"templateID"`
	SandboxID          string  `json:"sandboxID"`
	Alias              string  `json:"alias,omitempty"`
	ClientID           string  `json:"clientID"` // deprecated in E2B, still emitted
	EnvdVersion        string  `json:"envdVersion"`
	EnvdAccessToken    string  `json:"envdAccessToken,omitempty"`
	TrafficAccessToken *string `json:"trafficAccessToken,omitempty"`
	Domain             *string `json:"domain,omitempty"`
}

// ListedSandbox is one element of the GET /sandboxes response array.
type ListedSandbox struct {
	TemplateID  string            `json:"templateID"`
	SandboxID   string            `json:"sandboxID"`
	Alias       string            `json:"alias,omitempty"`
	ClientID    string            `json:"clientID"`
	StartedAt   time.Time         `json:"startedAt"`
	EndAt       time.Time         `json:"endAt"`
	CPUCount    int32             `json:"cpuCount"`
	MemoryMB    int32             `json:"memoryMB"`
	DiskSizeMB  int32             `json:"diskSizeMB"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	State       SandboxState      `json:"state"`
	EnvdVersion string            `json:"envdVersion"`
}

// SandboxDetail is the GET /sandboxes/{id} response.
type SandboxDetail struct {
	TemplateID          string            `json:"templateID"`
	SandboxID           string            `json:"sandboxID"`
	Alias               string            `json:"alias,omitempty"`
	ClientID            string            `json:"clientID"`
	StartedAt           time.Time         `json:"startedAt"`
	EndAt               time.Time         `json:"endAt"`
	EnvdVersion         string            `json:"envdVersion"`
	EnvdAccessToken     string            `json:"envdAccessToken,omitempty"`
	AllowInternetAccess *bool             `json:"allowInternetAccess,omitempty"`
	Domain              *string           `json:"domain,omitempty"`
	CPUCount            int32             `json:"cpuCount"`
	MemoryMB            int32             `json:"memoryMB"`
	DiskSizeMB          int32             `json:"diskSizeMB"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	State               SandboxState      `json:"state"`
}

// SandboxState mirrors E2B's enum.
type SandboxState string

const (
	StateRunning SandboxState = "running"
	StatePaused  SandboxState = "paused"
)

// SetTimeoutRequest is POST /sandboxes/{id}/timeout.
type SetTimeoutRequest struct {
	Timeout int32 `json:"timeout"`
}

// RefreshRequest is POST /sandboxes/{id}/refreshes.
type RefreshRequest struct {
	Duration int32 `json:"duration,omitempty"`
}

// ResumeRequest is POST /sandboxes/{id}/resume.
type ResumeRequest struct {
	Timeout int32 `json:"timeout,omitempty"`
}

// apiError is E2B's error envelope (spec Error schema: {code, message}).
type apiError struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
}

// DefaultEnvdVersion is what we report for the in-sandbox daemon. Chosen high
// enough that the SDK's version gates take the modern code paths.
const DefaultEnvdVersion = "0.2.0"

// DefaultTimeoutSeconds mirrors E2B's create default.
const DefaultTimeoutSeconds int32 = 15
