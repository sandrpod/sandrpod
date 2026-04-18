// Copyright 2024 SandrPod
// Provider interface - 抽象层接口定义

package provider

import (
	"context"
	"time"
)

// VMState VM 状态
type VMState string

const (
	VMStatePending    VMState = "PENDING"
	VMStateRunning    VMState = "RUNNING"
	VMStateStopping   VMState = "STOPPING"
	VMStateStopped    VMState = "STOPPED"
	VMStateTerminated VMState = "TERMINATED"
	VMStateError      VMState = "ERROR"
)

// Resources 资源配置
type Resources struct {
	CPU       float64 // CPU 核心数
	MemoryGiB float64 // 内存 GB
	DiskGiB   float64 // 磁盘 GB
	GPU       int      // GPU 数量
	GPUType   string   // GPU 类型，如 "NVIDIA T4"
}

// NetworkConfig 网络配置
type NetworkConfig struct {
	VpcID         string // VPC ID
	SubnetID      string // 子网 ID
	SecurityGroup string // 安全组
	PublicIP      bool   // 是否分配公网 IP
}

// DiskConfig 磁盘配置
type DiskConfig struct {
	SizeGiB    int    // 大小 GB
	VolumeType string // 卷类型: gp3, io2, standard
	Encrypted  bool   // 是否加密
}

// InstanceType 实例类型
type InstanceType struct {
	Name         string  // 实例名: t3.medium, Standard_D2s_v3
	CPU          float64 // CPU 核心数
	MemoryGiB    float64 // 内存 GB
	DiskGiB      float64 // 本地磁盘 GB (0 表示云盘)
	GPU          int     // GPU 数量
	GPUType      string  // GPU 类型
	PricePerHour float64 // 按小时价格 (美元)
}

// VMInfo VM 信息
type VMInfo struct {
	ID           string    // VM ID (云厂商)
	Name         string    // 名称
	Region       string    // 区域
	InstanceType string    // 实例类型
	State        VMState   // 状态
	PublicIP     string    // 公网 IP
	PrivateIP    string    // 私网 IP
	CreatedAt    time.Time // 创建时间
}

// HealthStatus 健康状态
type HealthStatus struct {
	VMReady      bool // VM 运行中
	DockerReady  bool // Docker 已安装
	PoderReady   bool // Poder 服务已启动
	APIReachable bool // API 可访问
}

// CommandResult 命令执行结果
type CommandResult struct {
	Output    string    // 标准输出
	ExitCode  int      // 退出码
	Stderr    string    // 错误输出
	ExecutedAt time.Time // 执行时间
}

// RunnerBootstrapConfig Poder 启动配置
type RunnerBootstrapConfig struct {
	APIURL       string // SandrPod API 地址
	APIKey       string // API 认证密钥
	PoderVersion string // Poder 版本
	LogLevel     string // 日志级别
}

// CreateVMRequest 创建 VM 请求
type CreateVMRequest struct {
	Name          string            // 名称
	Region        string            // 区域
	InstanceType  string            // 实例类型
	ImageID       string            // 镜像 ID (可选，使用默认)
	NetworkConfig *NetworkConfig   // 网络配置 (可选)
	DiskConfig    *DiskConfig      // 磁盘配置 (可选)
	RunnerConfig  *RunnerBootstrapConfig // Poder 启动配置
	Tags          map[string]string // 标签
}

// Provider 云厂商抽象接口
type Provider interface {
	// 元信息
	Name() string        // 云厂商标识: aws, azure, gcp, aliyun
	DisplayName() string // 显示名称: Amazon Web Services

	// VM 生命周期
	CreateVM(ctx context.Context, req *CreateVMRequest) (*VMInfo, error)
	DeleteVM(ctx context.Context, vmID string) error
	GetVM(ctx context.Context, vmID string) (*VMInfo, error)
	ListVMs(ctx context.Context) ([]*VMInfo, error)

	// 远程执行 (用于在 VM 上安装软件)
	ExecuteCommand(ctx context.Context, vmID, command string) (*CommandResult, error)

	// 健康检查
	WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error
	GetHealthStatus(ctx context.Context, vmID string) (*HealthStatus, error)

	// 基础设施查询
	ListRegions(ctx context.Context) ([]string, error)
	ListInstanceTypes(ctx context.Context, region string) ([]*InstanceType, error)
	GetDefaultImage(ctx context.Context, region string) (string, error)

	// 清理
	Cleanup(ctx context.Context) error
}
