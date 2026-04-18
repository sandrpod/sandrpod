// Copyright 2024 SandrPod
// Job 模型 - 用于 API Server 和 Poder Agent 之间的任务传递

package sandpod

import (
	"time"
)

// JobType 任务类型
type JobType string

const (
	JobTypeCreateSandbox  JobType = "CREATE_SANDBOX"
	JobTypeStartSandbox   JobType = "START_SANDBOX"
	JobTypeStopSandbox    JobType = "STOP_SANDBOX"
	JobTypeDeleteSandbox  JobType = "DELETE_SANDBOX"
	JobTypeExecuteCommand JobType = "EXECUTE_COMMAND"
)

// JobStatus 任务状态
type JobStatus string

const (
	JobStatusPending    JobStatus = "PENDING"
	JobStatusInProgress JobStatus = "IN_PROGRESS"
	JobStatusCompleted  JobStatus = "COMPLETED"
	JobStatusFailed     JobStatus = "FAILED"
)

// Job 任务模型
type Job struct {
	ID            string            `json:"id"`
	Type          JobType          `json:"type"`
	Status        JobStatus        `json:"status"`
	SandboxName   string            `json:"sandbox_name"`
	SandboxID     string            `json:"sandbox_id,omitempty"` // 实际的容器/VM ID
	Region        string            `json:"region"`
	ProviderType  string            `json:"provider_type,omitempty"` // provider 类型: aws, aliyun, local
	PoderID       string            `json:"poder_id,omitempty"`      // 目标 Poder ID (用于 Agent 创建容器)
	PoderURL      string            `json:"poder_url,omitempty"`     // 目标 Poder URL
	VmID          string            `json:"vm_id,omitempty"`        // 创建的 VM ID (云环境)
	InstanceType  string            `json:"instance_type"`
	ImageID       string            `json:"image_id,omitempty"`
	Command       string            `json:"command,omitempty"` // 代码或命令
	Language      string            `json:"language,omitempty"` // python, node, bash
	Result        *JobResult        `json:"result,omitempty"`
	ErrorMessage  string            `json:"error_message,omitempty"`
	TraceContext  map[string]string `json:"trace_context,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// JobResult 任务结果
type JobResult struct {
	SandboxID   string `json:"sandbox_id,omitempty"`
	IP          string `json:"ip,omitempty"`
	ProxyURL    string `json:"proxy_url,omitempty"` // Worker Proxy URL
	Output      string `json:"output,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
}

// CreateSandboxJobPayload 创建沙箱任务负载
type CreateSandboxJobPayload struct {
	Name          string `json:"name"`
	Region        string `json:"region"`
	ProviderType  string `json:"provider_type"`  // provider 类型: aws, aliyun, local
	InstanceType  string `json:"instance_type"`
	ImageID       string `json:"image_id"`
}

// ExecuteCommandPayload 执行命令负载
type ExecuteCommandPayload struct {
	SandboxName string `json:"sandbox_name"`
	Command     string `json:"command"`
}