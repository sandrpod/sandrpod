// Copyright 2026 SandrPod Contributors
// Job model - used for task passing between the API Server and Poder Agent

package sandpod

import (
	"time"
)

// JobType represents the type of a job.
type JobType string

const (
	JobTypeCreateSandbox  JobType = "CREATE_SANDBOX"
	JobTypeStartSandbox   JobType = "START_SANDBOX"
	JobTypeStopSandbox    JobType = "STOP_SANDBOX"
	JobTypeDeleteSandbox  JobType = "DELETE_SANDBOX"
	JobTypeExecuteCommand JobType = "EXECUTE_COMMAND"
)

// JobStatus represents the execution state of a job.
type JobStatus string

const (
	JobStatusPending    JobStatus = "PENDING"
	JobStatusInProgress JobStatus = "IN_PROGRESS"
	JobStatusCompleted  JobStatus = "COMPLETED"
	JobStatusFailed     JobStatus = "FAILED"
)

// Job is the job model.
type Job struct {
	ID           string     `json:"id"`
	Type         JobType    `json:"type"`
	Status       JobStatus  `json:"status"`
	SandboxName  string     `json:"sandbox_name"`
	SandboxID    string     `json:"sandbox_id,omitempty"` // actual container/VM ID
	Region       string     `json:"region"`
	ProviderType string     `json:"provider_type,omitempty"` // provider type: aws, aliyun, local
	PoderID      string     `json:"poder_id,omitempty"`      // target Poder ID (used by Agent to create containers)
	PoderURL     string     `json:"poder_url,omitempty"`     // target Poder URL
	VmID         string     `json:"vm_id,omitempty"`         // created VM ID (cloud environments)
	InstanceType string     `json:"instance_type"`
	ImageID      string     `json:"image_id,omitempty"`
	Command      string     `json:"command,omitempty"`  // code or command to execute
	Language     string     `json:"language,omitempty"` // language: python, node, bash
	Result       *JobResult `json:"result,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	// Owner is the auth-token name that initiated the job (see SandboxInfo.Owner).
	Owner        string            `json:"owner,omitempty"`
	TraceContext map[string]string `json:"trace_context,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// JobResult holds the outcome of a completed job.
type JobResult struct {
	SandboxID string `json:"sandbox_id,omitempty"`
	IP        string `json:"ip,omitempty"`
	ProxyURL  string `json:"proxy_url,omitempty"` // worker proxy URL
	Output    string `json:"output,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
}

// CreateSandboxJobPayload is the payload for a CREATE_SANDBOX job.
type CreateSandboxJobPayload struct {
	Name         string `json:"name"`
	Region       string `json:"region"`
	ProviderType string `json:"provider_type"` // provider type: aws, aliyun, local
	InstanceType string `json:"instance_type"`
	ImageID      string `json:"image_id"`
}

// ExecuteCommandPayload is the payload for an EXECUTE_COMMAND job.
type ExecuteCommandPayload struct {
	SandboxName string `json:"sandbox_name"`
	Command     string `json:"command"`
}
