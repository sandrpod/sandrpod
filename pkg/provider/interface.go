// Copyright 2024 SandrPod
// Provider interface - abstract layer interface definitions

package provider

import (
	"context"
	"time"
)

// VMState represents the state of a VM.
type VMState string

const (
	VMStatePending    VMState = "PENDING"
	VMStateRunning    VMState = "RUNNING"
	VMStateStopping   VMState = "STOPPING"
	VMStateStopped    VMState = "STOPPED"
	VMStateTerminated VMState = "TERMINATED"
	VMStateError      VMState = "ERROR"
)

// Resources defines compute resource requirements.
type Resources struct {
	CPU       float64 // Number of CPU cores
	MemoryGiB float64 // Memory in GiB
	DiskGiB   float64 // Disk in GiB
	GPU       int      // Number of GPUs
	GPUType   string   // GPU model, e.g. "NVIDIA T4"
}

// NetworkConfig holds network configuration for a VM.
type NetworkConfig struct {
	VpcID         string // VPC ID
	SubnetID      string // Subnet ID
	SecurityGroup string // Security group
	PublicIP      bool   // Whether to assign a public IP
}

// DiskConfig holds disk configuration for a VM.
type DiskConfig struct {
	SizeGiB    int    // Disk size in GiB
	VolumeType string // Volume type: gp3, io2, standard
	Encrypted  bool   // Whether to encrypt the disk
}

// InstanceType describes a cloud instance type.
type InstanceType struct {
	Name         string  // Instance name, e.g. t3.medium, Standard_D2s_v3
	CPU          float64 // Number of CPU cores
	MemoryGiB    float64 // Memory in GiB
	DiskGiB      float64 // Local disk in GiB (0 means cloud disk)
	GPU          int     // Number of GPUs
	GPUType      string  // GPU model
	PricePerHour float64 // Hourly price in USD
}

// VMInfo holds information about a VM instance.
type VMInfo struct {
	ID           string    // VM ID assigned by the cloud provider
	Name         string    // Instance name
	Region       string    // Region
	InstanceType string    // Instance type
	State        VMState   // Current state
	PublicIP     string    // Public IP address
	PrivateIP    string    // Private IP address
	CreatedAt    time.Time // Creation timestamp
}

// HealthStatus reports the health of a VM and its services.
type HealthStatus struct {
	VMReady      bool // VM is running
	DockerReady  bool // Docker is installed and running
	PoderReady   bool // Poder service is started
	APIReachable bool // API endpoint is reachable
}

// CommandResult holds the output of a remotely executed command.
type CommandResult struct {
	Output    string    // Standard output
	ExitCode  int      // Exit code
	Stderr    string    // Standard error output
	ExecutedAt time.Time // Execution timestamp
}

// RunnerBootstrapConfig holds Poder bootstrap configuration.
type RunnerBootstrapConfig struct {
	APIURL       string // SandrPod API URL
	APIKey       string // API authentication key
	PoderVersion string // Poder version
	LogLevel     string // Log level
}

// CreateVMRequest is the request payload for creating a VM.
type CreateVMRequest struct {
	Name          string            // Instance name
	Region        string            // Region
	InstanceType  string            // Instance type
	ImageID       string            // Image ID (optional, uses default if empty)
	NetworkConfig *NetworkConfig   // Network configuration (optional)
	DiskConfig    *DiskConfig      // Disk configuration (optional)
	RunnerConfig  *RunnerBootstrapConfig // Poder bootstrap configuration
	Tags          map[string]string // Resource tags
}

// Provider is the abstract interface for a cloud provider.
type Provider interface {
	// Metadata
	Name() string        // Provider identifier: aws, azure, gcp, aliyun
	DisplayName() string // Human-readable name: Amazon Web Services

	// VM lifecycle
	CreateVM(ctx context.Context, req *CreateVMRequest) (*VMInfo, error)
	DeleteVM(ctx context.Context, vmID string) error
	GetVM(ctx context.Context, vmID string) (*VMInfo, error)
	ListVMs(ctx context.Context) ([]*VMInfo, error)

	// Remote execution (used for bootstrapping software on the VM)
	ExecuteCommand(ctx context.Context, vmID, command string) (*CommandResult, error)

	// Health checks
	WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error
	GetHealthStatus(ctx context.Context, vmID string) (*HealthStatus, error)

	// Infrastructure queries
	ListRegions(ctx context.Context) ([]string, error)
	ListInstanceTypes(ctx context.Context, region string) ([]*InstanceType, error)
	GetDefaultImage(ctx context.Context, region string) (string, error)

	// Cleanup
	Cleanup(ctx context.Context) error
}
