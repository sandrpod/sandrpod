// Copyright 2024 SandrPod
// Poder core interface - modeled after the Daytona Runner

package poder

import (
	"context"
	"time"
)

// PodState represents the lifecycle state of a pod.
type PodState string

const (
	PodStatePending   PodState = "PENDING"
	PodStateStarting  PodState = "STARTING"
	PodStateRunning   PodState = "RUNNING"
	PodStateStopping  PodState = "STOPPING"
	PodStateStopped   PodState = "STOPPED"
	PodStateError     PodState = "ERROR"
)

// PodInfo holds information about a running pod.
type PodInfo struct {
	ID           string    // Pod ID (Sandbox ID)
	Name         string    // Pod name
	Region       string    // Region
	Provider     string    // Cloud provider
	InstanceType string    // Instance type
	State        PodState  // Current state
	IP           string    // Container IP address
	CreatedAt    time.Time // Creation timestamp
	LastActivity time.Time // Last activity timestamp
}

// CreatePodRequest is the request payload for creating a pod.
//
// JSON tags are required because sandpod-server marshals an upstream
// CreateSandboxRequest (which has snake_case json tags) and forwards the
// JSON over the tunnel to this struct on the Poder side. Without explicit
// tags, Go's case-insensitive default field matching fails on snake_case
// keys like "image_id" → "ImageID" (underscore breaks the match), so the
// field silently stays at its zero value and the Poder docker driver
// fell back to the default toolbox image regardless of caller intent.
type CreatePodRequest struct {
	Name         string            `json:"name"`
	Region       string            `json:"region,omitempty"`
	InstanceType string            `json:"instance_type,omitempty"`
	ImageID      string            `json:"image_id,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	NetworkConfig *NetworkConfig   `json:"network_config,omitempty"`
	DiskConfig   *DiskConfig       `json:"disk_config,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	// Bootstrap configuration
	APIURL       string `json:"api_url,omitempty"`       // Toolbox API URL
	PoderVersion string `json:"poder_version,omitempty"` // Poder version
	LogLevel     string `json:"log_level,omitempty"`     // Log level
}

// NetworkConfig holds network configuration for a pod.
type NetworkConfig struct {
	VpcID         string `json:"vpc_id,omitempty"`
	SubnetID      string `json:"subnet_id,omitempty"`
	SecurityGroup string `json:"security_group,omitempty"`
}

// DiskConfig holds disk configuration for a pod.
type DiskConfig struct {
	SizeGiB    int    `json:"size_gib,omitempty"`
	VolumeType string `json:"volume_type,omitempty"`
}

// CommandResult holds the output of a remotely executed command.
type CommandResult struct {
	Output    string    // Standard output
	ExitCode  int       // Exit code
	Stderr    string    // Standard error output
	ExecutedAt time.Time // Execution timestamp
}

// HealthStatus reports the health of a pod and its services.
type HealthStatus struct {
	PodReady     bool // Pod is running
	DockerReady  bool // Docker is installed and running
	ToolboxReady bool // Toolbox service is started
	APIReachable bool // API endpoint is reachable
}

// Poder is the interface for a pod executor.
type Poder interface {
	// Metadata
	Name() string        // Poder identifier: aws-runner, azure-runner
	DisplayName() string // Human-readable display name
	Region() string      // Region this Poder manages

	// Pod lifecycle
	CreatePod(ctx context.Context, req *CreatePodRequest) (*PodInfo, error)
	DeletePod(ctx context.Context, podID string) error
	GetPod(ctx context.Context, podID string) (*PodInfo, error)
	ListPods(ctx context.Context) ([]*PodInfo, error)

	// Pod pause/resume (used for Start/Stop)
	PausePod(ctx context.Context, podID string) error
	UnpausePod(ctx context.Context, podID string) error

	// Remote execution
	ExecuteCommand(ctx context.Context, podID, command string) (*CommandResult, error)

	// Health checks
	WaitUntilRunning(ctx context.Context, podID string, timeout time.Duration) error
	GetHealthStatus(ctx context.Context, podID string) (*HealthStatus, error)

	// Logs
	GetPodLogs(ctx context.Context, podID string, tail string) (string, error)

	// Cleanup
	Cleanup(ctx context.Context) error
}
