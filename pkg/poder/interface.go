// Copyright 2024 SandrPod
// Poder 核心接口 - 对标 Daytona Runner

package poder

import (
	"context"
	"time"
)

// PodState Pod 状态
type PodState string

const (
	PodStatePending   PodState = "PENDING"
	PodStateStarting  PodState = "STARTING"
	PodStateRunning   PodState = "RUNNING"
	PodStateStopping  PodState = "STOPPING"
	PodStateStopped   PodState = "STOPPED"
	PodStateError     PodState = "ERROR"
)

// PodInfo Pod 信息
type PodInfo struct {
	ID           string    // Pod ID (Sandbox ID)
	Name         string    // 名称
	Region       string    // 区域
	Provider     string    // 云厂商
	InstanceType string    // 实例类型
	State        PodState  // 状态
	IP           string    // 容器 IP
	CreatedAt    time.Time // 创建时间
	LastActivity time.Time // 最后活动时间
}

// ToolboxInfo Toolbox 信息
type ToolboxInfo struct {
	APIURL      string // Toolbox API 地址
	APIToken    string // API 认证 Token
	SSHPort     int    // SSH 端口
	SSHUser     string // SSH 用户
	SSHPassword string // SSH 密码
}

// CreatePodRequest 创建 Pod 请求
type CreatePodRequest struct {
	Name         string            // 名称
	Region       string            // 区域
	InstanceType string            // 实例类型
	ImageID      string            // 镜像 ID
	Provider     string            // 云厂商
	NetworkConfig *NetworkConfig   // 网络配置
	DiskConfig   *DiskConfig       // 磁盘配置
	Labels       map[string]string // 标签
	// 启动配置
	APIURL       string // Toolbox API 地址
	PoderVersion string // Poder 版本
	LogLevel     string // 日志级别
}

// NetworkConfig 网络配置
type NetworkConfig struct {
	VpcID         string // VPC ID
	SubnetID      string // 子网 ID
	SecurityGroup string // 安全组
}

// DiskConfig 磁盘配置
type DiskConfig struct {
	SizeGiB    int    // 大小 GB
	VolumeType string // 卷类型
}

// CommandResult 命令执行结果
type CommandResult struct {
	Output    string    // 标准输出
	ExitCode  int       // 退出码
	Stderr    string    // 错误输出
	ExecutedAt time.Time // 执行时间
}

// HealthStatus 健康状态
type HealthStatus struct {
	PodReady     bool // Pod 运行中
	DockerReady  bool // Docker 已安装
	ToolboxReady bool // Toolbox 服务已启动
	APIReachable bool // API 可访问
}

// Poder Pod 执行器接口
type Poder interface {
	// 元信息
	Name() string        // Poder 标识: aws-runner, azure-runner
	DisplayName() string // 显示名称
	Region() string      // 所属区域

	// Pod 生命周期
	CreatePod(ctx context.Context, req *CreatePodRequest) (*PodInfo, error)
	DeletePod(ctx context.Context, podID string) error
	GetPod(ctx context.Context, podID string) (*PodInfo, error)
	ListPods(ctx context.Context) ([]*PodInfo, error)

	// Pod 暂停/恢复 (用于 Start/Stop)
	PausePod(ctx context.Context, podID string) error
	UnpausePod(ctx context.Context, podID string) error

	// 远程执行
	ExecuteCommand(ctx context.Context, podID, command string) (*CommandResult, error)

	// 健康检查
	WaitUntilRunning(ctx context.Context, podID string, timeout time.Duration) error
	GetHealthStatus(ctx context.Context, podID string) (*HealthStatus, error)

	// Toolbox 信息
	GetToolboxInfo(ctx context.Context, podID string) (*ToolboxInfo, error)

	// 日志
	GetPodLogs(ctx context.Context, podID string, tail string) (string, error)

	// 清理
	Cleanup(ctx context.Context) error
}
