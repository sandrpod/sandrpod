// Copyright 2024 SandrPod
// SandPod 接口定义 - 对标 Daytona Sandbox

package sandpod

import (
	"context"
	"time"
)

// State SandPod 状态
type State string

const (
	StatePending    State = "PENDING"    // 等待创建
	StateStarting   State = "STARTING"  // 启动中
	StateRunning    State = "RUNNING"   // 运行中
	StateStopping   State = "STOPPING"  // 停止中
	StateStopped    State = "STOPPED"   // 已停止
	StateError      State = "ERROR"     // 错误
	StateTerminated State = "TERMINATED" // 已终止
)

// DesiredState 期望状态
type DesiredState string

const (
	DesiredStateRunning  DesiredState = "RUNNING"  // 期望运行
	DesiredStateStopped  DesiredState = "STOPPED"  // 期望停止
	DesiredStateTerminate DesiredState = "TERMINATE" // 期望终止
)

// SandPodInfo SandPod 信息
type SandPodInfo struct {
	ID            string       // SandPod ID
	Name          string       // 名称
	Region       string       // 区域
	Provider     string       // 云厂商
	InstanceType string       // 实例类型
	ImageID      string       // 镜像 ID
	State        State        // 当前状态
	DesiredState DesiredState // 期望状态
	IP           string       // IP 地址
	APIURL       string       // Toolbox API URL
	SSHPort      int          // SSH 端口
	CreatedAt    time.Time    // 创建时间
	LastActivity time.Time    // 最后活动时间
	Labels       map[string]string // 标签
}

// CreateSandPodRequest 创建 SandPod 请求
type CreateSandPodRequest struct {
	Name         string            // 名称
	Region       string            // 区域
	InstanceType string            // 实例类型
	ImageID      string            // 镜像 ID (可选)
	Provider     string            // 云厂商 (可选)
	NetworkConfig *NetworkConfig   // 网络配置
	Labels       map[string]string // 标签
}

// NetworkConfig 网络配置
type NetworkConfig struct {
	VpcID         string
	SubnetID      string
	SecurityGroup string
}

// ProcessRequest 进程执行请求
type ProcessRequest struct {
	Lang    string // python, node, bash
	Code    string // 代码内容
	Timeout int    // 超时时间(秒)
}

// ProcessResult 进程执行结果
type ProcessResult struct {
	ExitCode int       // 退出码
	Stdout   string    // 标准输出
	Stderr   string    // 错误输出
	StartedAt time.Time // 开始时间
	EndedAt  time.Time // 结束时间
}

// HealthStatus 健康状态
type HealthStatus struct {
	PodReady     bool // Pod 运行中
	DockerReady  bool // Docker 已安装
	ToolboxReady bool // Toolbox 服务已启动
	APIReachable bool // API 可访问
}

// SandPod SandPod 接口
type SandPod interface {
	// 元信息
	ID() string
	Name() string

	// 状态
	GetState() State
	GetDesiredState() DesiredState
	SetDesiredState(state DesiredState) error

	// 生命周期
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Delete(ctx context.Context) error

	// 执行
	Process(ctx context.Context, req *ProcessRequest) (*ProcessResult, error)

	// 健康检查
	GetHealthStatus(ctx context.Context) (*HealthStatus, error)

	// 信息
	GetInfo(ctx context.Context) (*SandPodInfo, error)
}
