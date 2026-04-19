// Copyright 2024 SandrPod
// Docker Poder 实现 - 用于本地开发和测试

package poder

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerPoder Docker 实现
type DockerPoder struct {
	*BasePoder
	dockerClient *client.Client
	networkName  string
}

// NewDockerPoder 创建 Docker Poder。
// networkName 指定沙箱容器加入的 Docker 网络；空字符串则从环境变量
// SANDRPOD_NETWORK 读取；仍为空时不指定网络，容器使用 Docker 默认网络（bridge）。
func NewDockerPoder(region, networkName string) (*DockerPoder, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	base := NewBasePoder("docker", "Docker Provider", region)

	if networkName == "" {
		networkName = os.Getenv("SANDRPOD_NETWORK")
	}
	// networkName 仍为空 → 使用 Docker 默认网络，不强制指定

	return &DockerPoder{
		BasePoder:    base,
		dockerClient: dockerClient,
		networkName:  networkName,
	}, nil
}

// EnsureNetwork 确保网络存在
func (p *DockerPoder) EnsureNetwork(ctx context.Context) error {
	// 检查网络是否存在
	networks, err := p.dockerClient.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return err
	}

	for _, net := range networks {
		if net.Name == p.networkName {
			return nil
		}
	}

	// 创建网络
	_, err = p.dockerClient.NetworkCreate(ctx, p.networkName, network.CreateOptions{
		Driver: "bridge",
	})
	return err
}

// generateSandboxPassword 生成随机密码
func generateSandboxPassword() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// CreatePod 创建 Pod
func (p *DockerPoder) CreatePod(ctx context.Context, req *CreatePodRequest) (*PodInfo, error) {
	// 只有指定了自定义网络才需要确保网络存在
	if p.networkName != "" {
		if err := p.EnsureNetwork(ctx); err != nil {
			return nil, fmt.Errorf("failed to ensure network: %w", err)
		}
	}

	// 生成 Pod ID
	podID := fmt.Sprintf("sp-%d-%s", time.Now().Unix(), randomString(8))

	// 镜像
	imageName := req.ImageID
	if imageName == "" {
		imageName = os.Getenv("SANDRPOD_TOOLBOX_IMAGE")
		if imageName == "" {
			imageName = "sandrpod/toolbox:test"
		}
	}

	// 创建容器
	labels := req.Labels
	if labels == nil {
		labels = make(map[string]string)
	}
	labels["sandrpod/sandbox-name"] = req.Name

	containerConfig := &container.Config{
		Image: imageName,
		Env: []string{
			"TOOLBOX_API_URL=" + req.APIURL,
			"PODER_VERSION=" + req.PoderVersion,
			"LOG_LEVEL=" + req.LogLevel,
		},
		Labels: labels,
	}

	hostConfig := &container.HostConfig{}
	if p.networkName != "" {
		hostConfig.NetworkMode = container.NetworkMode(p.networkName)
	}

	resp, err := p.dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, podID)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// 启动容器
	if err := p.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// 获取容器信息
	containerInfo, err := p.dockerClient.ContainerInspect(ctx, resp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	// 获取容器 IP：优先从指定网络取，未指定时取第一个可用网络的 IP
	ip := ""
	if p.networkName != "" {
		if netSettings, ok := containerInfo.NetworkSettings.Networks[p.networkName]; ok {
			ip = netSettings.IPAddress
		}
	} else {
		for _, netSettings := range containerInfo.NetworkSettings.Networks {
			if netSettings.IPAddress != "" {
				ip = netSettings.IPAddress
				break
			}
		}
	}

	// 创建 PodInfo
	pod := &PodInfo{
		ID:           podID,
		Name:         req.Name,
		Region:       req.Region,
		Provider:     "docker",
		InstanceType: req.InstanceType,
		State:        PodStateStarting,
		IP:           ip,
		CreatedAt:    time.Now(),
	}

	p.RegisterPod(pod)

	// 后台等待 Running
	go func() {
		time.Sleep(5 * time.Second)
		p.UpdatePodState(podID, PodStateRunning)
		pod.State = PodStateRunning
	}()

	return pod, nil
}

// DeletePod 删除 Pod
func (p *DockerPoder) DeletePod(ctx context.Context, podID string) error {
	// 停止容器
	_ = p.dockerClient.ContainerStop(ctx, podID, container.StopOptions{Timeout: ptr(10)})

	// 删除容器
	if err := p.dockerClient.ContainerRemove(ctx, podID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	p.UnregisterPod(podID)
	return nil
}

// PausePod 暂停 Pod (保持容器状态)
func (p *DockerPoder) PausePod(ctx context.Context, podID string) error {
	return p.dockerClient.ContainerPause(ctx, podID)
}

// UnpausePod 恢复 Pod
func (p *DockerPoder) UnpausePod(ctx context.Context, podID string) error {
	return p.dockerClient.ContainerUnpause(ctx, podID)
}

// GetPod 获取 Pod
func (p *DockerPoder) GetPod(ctx context.Context, podID string) (*PodInfo, error) {
	// 先尝试从内部注册表获取
	pod, ok := p.GetPodByID(podID)
	if !ok {
		// 注册表中没有，尝试直接从 Docker 查询容器
		info, err := p.dockerClient.ContainerInspect(ctx, podID)
		if err != nil {
			return nil, fmt.Errorf("pod %s not found", podID)
		}
		// 构建 PodInfo
		pod = &PodInfo{
			ID:        info.ID,
			Name:      info.Name,
			State:     PodStateStopped,
		}
		// 从网络设置获取 IP
		if p.networkName != "" {
			if net, ok := info.NetworkSettings.Networks[p.networkName]; ok {
				pod.IP = net.IPAddress
			}
		} else {
			for _, net := range info.NetworkSettings.Networks {
				if net.IPAddress != "" {
					pod.IP = net.IPAddress
					break
				}
			}
		}
	}

	// 查询 Docker 容器状态
	info, err := p.dockerClient.ContainerInspect(ctx, podID)
	if err != nil {
		return pod, nil // 返回缓存的状态
	}

	// Paused must be checked before Running, since paused containers have Running=true
	if info.State.Paused {
		pod.State = PodStateStopped
	} else if info.State.Running {
		pod.State = PodStateRunning
	} else if info.State.ExitCode != 0 {
		pod.State = PodStateError
	} else {
		pod.State = PodStateStopped
	}

	return pod, nil
}

// FindPodByName 根据沙箱名称查找 Pod (查询 Docker 直接查找)
func (p *DockerPoder) FindPodByName(ctx context.Context, sandboxName string) (*PodInfo, error) {
	containers, err := p.dockerClient.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range containers {
		if c.Labels["sandrpod/sandbox-name"] == sandboxName {
			return p.GetPod(ctx, c.ID)
		}
	}

	return nil, fmt.Errorf("pod with sandbox name %s not found", sandboxName)
}

// GetPodLogs 获取容器日志
func (p *DockerPoder) GetPodLogs(ctx context.Context, podID string, tail string) (string, error) {
	if tail == "" {
		tail = "100"
	}

	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Tail:       tail,
	}

	resp, err := p.dockerClient.ContainerLogs(ctx, podID, options)
	if err != nil {
		return "", fmt.Errorf("failed to get container logs: %w", err)
	}
	defer resp.Close()

	// 解码日志流 (Docker logs 使用 multiplexed 格式)
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, resp)
	if err != nil {
		return "", fmt.Errorf("failed to copy logs: %w", err)
	}

	// 组合 stdout 和 stderr
	logs := stdout.String()
	if stderr.Len() > 0 {
		logs += stderr.String()
	}

	return logs, nil
}

// ExecuteCommand 执行命令
func (p *DockerPoder) ExecuteCommand(ctx context.Context, podID, command string) (*CommandResult, error) {
	execResp, err := p.dockerClient.ContainerExecCreate(ctx, podID, container.ExecOptions{
		Cmd:          []string{"/bin/sh", "-c", command},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create exec: %w", err)
	}

	resp, err := p.dockerClient.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to attach exec: %w", err)
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, resp.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to copy output: %w", err)
	}

	// 获取退出码
	info, err := p.dockerClient.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec: %w", err)
	}

	return &CommandResult{
		Output:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  int(info.ExitCode),
		ExecutedAt: time.Now(),
	}, nil
}

// ExecWithPty 创建带 PTY 的执行会话，返回 exec ID
func (p *DockerPoder) ExecWithPty(ctx context.Context, podID string, width, height int) (string, error) {
	fmt.Printf("ExecWithPty: starting for podID=%s\n", podID)

	execResp, err := p.dockerClient.ContainerExecCreate(ctx, podID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          []string{"/bin/bash", "-c", "echo started && cat"},
	})
	if err != nil {
		fmt.Printf("ExecWithPty: create error: %v\n", err)
		return "", fmt.Errorf("failed to create exec: %w", err)
	}
	fmt.Printf("ExecWithPty: created execID=%s\n", execResp.ID)

	// 在后台启动 exec
	go func() {
		fmt.Printf("ExecWithPty: goroutine starting for execID=%s\n", execResp.ID)
		err := p.dockerClient.ContainerExecStart(context.Background(), execResp.ID, container.ExecStartOptions{
			Tty: true,
		})
		if err != nil {
			fmt.Printf("ExecWithPty: start error (goroutine): %v\n", err)
		} else {
			fmt.Printf("ExecWithPty: goroutine completed for execID=%s\n", execResp.ID)
		}
	}()

	// 等待 exec 启动
	time.Sleep(200 * time.Millisecond)

	return execResp.ID, nil
}

// AttachExec 附加到执行中的进程，返回 HijackedResponse
func (p *DockerPoder) AttachExec(ctx context.Context, execID string) (*types.HijackedResponse, error) {
	resp, err := p.dockerClient.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("failed to attach to exec: %w", err)
	}

	return &resp, nil
}

// InspectExec 获取执行进程信息
func (p *DockerPoder) InspectExec(ctx context.Context, execID string) (*container.ExecInspect, error) {
	info, err := p.dockerClient.ContainerExecInspect(ctx, execID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec: %w", err)
	}
	return &info, nil
}

// GetHealthStatus 获取健康状态
func (p *DockerPoder) GetHealthStatus(ctx context.Context, podID string) (*HealthStatus, error) {
	pod, err := p.GetPod(ctx, podID)
	if err != nil {
		return nil, err
	}

	status := &HealthStatus{
		PodReady:     pod.State == PodStateRunning,
		DockerReady:  true,
		ToolboxReady: pod.State == PodStateRunning,
	}

	return status, nil
}

// WaitUntilRunning 等待 Pod 运行
func (p *DockerPoder) WaitUntilRunning(ctx context.Context, podID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Second)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %s to be running", podID)
		}

		pod, err := p.GetPod(ctx, podID)
		if err == nil && pod != nil && pod.State == PodStateRunning {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// GetToolboxInfo 获取 Toolbox 信息
func (p *DockerPoder) GetToolboxInfo(ctx context.Context, podID string) (*ToolboxInfo, error) {
	pod, err := p.GetPod(ctx, podID)
	if err != nil {
		return nil, err
	}

	if pod.State != PodStateRunning {
		return nil, fmt.Errorf("pod %s is not running", podID)
	}

	return &ToolboxInfo{
		APIURL:      fmt.Sprintf("http://%s:8080", pod.IP),
		APIToken:    "docker-token",
		SSHPort:     22220,
		SSHUser:     "daytona",
		SSHPassword: "sandbox-ssh",
	}, nil
}

// Cleanup 清理资源
func (p *DockerPoder) Cleanup(ctx context.Context) error {
	// 列出所有容器
	containers, err := p.dockerClient.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}

	// 删除所有 SandrPod 容器
	for _, c := range containers {
		if c.Labels["sandrpod"] == "true" {
			_ = p.dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		}
	}

	return nil
}

// randomString 生成随机字符串
func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

// ptr 返回指针
func ptr[T any](v T) *T {
	return &v
}
