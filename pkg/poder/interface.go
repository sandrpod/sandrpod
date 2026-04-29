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
type CreatePodRequest struct {
	Name         string            // Pod name
	Region       string            // Region
	InstanceType string            // Instance type
	ImageID      string            // Image ID
	Provider     string            // Cloud provider
	NetworkConfig *NetworkConfig   // Network configuration
	DiskConfig   *DiskConfig       // Disk configuration
	Labels       map[string]string // Labels
	// Bootstrap configuration
	APIURL       string // Toolbox API URL
	PoderVersion string // Poder version
	LogLevel     string // Log level
}

// NetworkConfig holds network configuration for a pod.
type NetworkConfig struct {
	VpcID         string // VPC ID
	SubnetID      string // Subnet ID
	SecurityGroup string // Security group
}

// DiskConfig holds disk configuration for a pod.
type DiskConfig struct {
	SizeGiB    int    // Disk size in GiB
	VolumeType string // Volume type
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
