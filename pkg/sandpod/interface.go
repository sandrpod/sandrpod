// Copyright 2024 SandrPod
// SandPod 状态类型定义

package sandpod

// State SandPod 沙箱状态
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
