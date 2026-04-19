// Copyright 2024 SandrPod
// Scheduler - Sandbox 创建调度编排逻辑
// 根据 Provider 类型调度：优先使用已有 Poder 资源，不够时创建新 VM

package sandpod

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// DefaultAPIURL 默认 API Server URL (本地开发用)
var DefaultAPIURL = "http://localhost:8080"

// Scheduler 调度器
type Scheduler struct {
	poderStore PoderRepository
	apiURL     string
}

// NewScheduler 创建调度器
func NewScheduler(poderStore PoderRepository, apiURL string) *Scheduler {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &Scheduler{
		poderStore: poderStore,
		apiURL:     apiURL,
	}
}

// ScheduleSandboxCreation 调度 Sandbox 创建
// 流程：
// 1. 查询指定 provider 类型的可用 Poder
// 2. 如有资源，直接在 Poder 创建
// 3. 如无资源，调用 Provider 创建 VM，等待 Poder 注册，再创建
func (s *Scheduler) ScheduleSandboxCreation(ctx context.Context, req *CreateSandboxRequest) (*Job, error) {
	providerType := req.ProviderType
	if providerType == "" {
		providerType = "local"
	}

	// 1. 查询可用 Poder
	poder, err := s.poderStore.SelectBest(req.Region, providerType)
	if err == nil && poder != nil {
		log.Printf("[Scheduler] Found available poder %s for provider %s", poder.ID, providerType)
		return s.createJobForPoder(req, poder)
	}

	// 2. 没有可用 Poder，需要先创建 VM (仅对云 provider 有效)
	if providerType == "local" || providerType == "docker" {
		return nil, fmt.Errorf("no available %s poder found", providerType)
	}

	log.Printf("[Scheduler] No available poder for %s, creating new VM", providerType)

	// 3. 调用 Provider 创建 VM
	vm, err := s.createVMWithProvider(ctx, providerType, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// 4. 等待 VM 就绪
	log.Printf("[Scheduler] Waiting for VM %s to be ready", vm.ID)
	if err := s.waitForVMReady(ctx, providerType, vm.ID); err != nil {
		return nil, fmt.Errorf("VM not ready: %w", err)
	}

	// 5. 在 VM 上安装 Docker 并启动 Poder
	log.Printf("[Scheduler] Setting up poder on VM %s", vm.ID)
	if err := s.setupPoderOnVM(ctx, providerType, vm); err != nil {
		return nil, fmt.Errorf("failed to setup poder: %w", err)
	}

	// 6. 等待 Poder 注册上线
	log.Printf("[Scheduler] Waiting for poder registration for VM %s", vm.ID)
	poder = s.waitForPoderRegistration(providerType, req.Region, 5*time.Minute)
	if poder == nil {
		return nil, fmt.Errorf("poder registration timeout")
	}

	log.Printf("[Scheduler] Poder %s registered successfully", poder.ID)

	// 7. 创建 Job
	return s.createJobForPoder(req, poder)
}

// createVMWithProvider 调用 Provider 创建 VM
func (s *Scheduler) createVMWithProvider(ctx context.Context, providerType string, req *CreateSandboxRequest) (*provider.VMInfo, error) {
	p := provider.GetFactory().MustGet(providerType)

	createReq := &provider.CreateVMRequest{
		Name:         fmt.Sprintf("sandrpod-%s", req.Name),
		Region:       req.Region,
		InstanceType: req.InstanceType,
		ImageID:      req.ImageID,
		Tags: map[string]string{
			"CreatedBy": "sandrpod",
			"Sandbox":   req.Name,
		},
		RunnerConfig: &provider.RunnerBootstrapConfig{
			APIURL: s.apiURL,
		},
	}

	vm, err := p.CreateVM(ctx, createReq)
	if err != nil {
		return nil, err
	}

	log.Printf("[Scheduler] VM %s created with IP %s", vm.ID, vm.PublicIP)
	return vm, nil
}

// waitForVMReady 等待 VM 就绪
func (s *Scheduler) waitForVMReady(ctx context.Context, providerType, vmID string) error {
	p := provider.GetFactory().MustGet(providerType)

	timeout := 5 * time.Minute
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		status, err := p.GetHealthStatus(ctx, vmID)
		if err != nil {
			log.Printf("[Scheduler] Error getting health status: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}

		if status.VMReady {
			return nil
		}

		log.Printf("[Scheduler] VM %s not ready yet, waiting...", vmID)
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timeout waiting for VM %s", vmID)
}

// setupPoderOnVM 在 VM 上安装 Docker 并启动 Poder 容器
func (s *Scheduler) setupPoderOnVM(ctx context.Context, providerType string, vm *provider.VMInfo) error {
	p := provider.GetFactory().MustGet(providerType)

	// 1. 安装 Docker
	installDocker := `curl -fsSL https://get.docker.com | sh`
	log.Printf("[Scheduler] Installing Docker on VM %s", vm.ID)

	result, err := p.ExecuteCommand(ctx, vm.ID, installDocker)
	if err != nil {
		return fmt.Errorf("failed to install docker: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("docker install failed: %s", result.Stderr)
	}

	// 2. 启动 Poder 容器
	// PROXY_HOST 应该是 VM 的公网 IP，供容器内服务访问外部使用
	poderStartCmd := fmt.Sprintf(
		`docker run -d \
			--name sandrpod-poder \
			--restart=always \
			-e API_URL=%s \
			-e PROXY_HOST=%s \
			-e REGION=%s \
			-v /var/run/docker.sock:/var/run/docker.sock \
			sandrpod/poder:latest`,
		s.apiURL,
		vm.PublicIP,
		vm.Region,
	)

	log.Printf("[Scheduler] Starting poder container on VM %s", vm.ID)

	result, err = p.ExecuteCommand(ctx, vm.ID, poderStartCmd)
	if err != nil {
		return fmt.Errorf("failed to start poder: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("poder start failed: %s", result.Stderr)
	}

	log.Printf("[Scheduler] Poder container started on VM %s", vm.ID)
	return nil
}

// waitForPoderRegistration 等待 Poder 注册上线
// 通过轮询 poderStore 检查是否有新的对应类型的 Poder 上线
func (s *Scheduler) waitForPoderRegistration(providerType, region string, timeout time.Duration) *PoderInfo {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// 查找新注册的 Poder
		// 这里简单处理：找第一个匹配 providerType 和 region 的 ONLINE Poder
		// 更精确的做法是用 vmID 关联，但当前 RegisterPoderRequest 没有 vmID 字段
		poder, err := s.poderStore.SelectBest(region, providerType)
		if err == nil && poder != nil {
			return poder
		}

		log.Printf("[Scheduler] Waiting for poder registration (provider=%s, region=%s)...", providerType, region)
		time.Sleep(5 * time.Second)
	}

	return nil
}

// createJobForPoder 为指定的 Poder 创建 Job
func (s *Scheduler) createJobForPoder(req *CreateSandboxRequest, poder *PoderInfo) (*Job, error) {
	job := &Job{
		ID:           GenerateJobID(),
		Type:         JobTypeCreateSandbox,
		Status:       JobStatusPending,
		SandboxName:  req.Name,
		Region:       req.Region,
		ProviderType: req.ProviderType,
		PoderID:      poder.ID,
		PoderURL:     poder.URL,
		InstanceType: req.InstanceType,
		ImageID:      req.ImageID,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	log.Printf("[Scheduler] Created job %s for poder %s (url=%s, provider=%s)",
		job.ID, poder.ID, poder.URL, req.ProviderType)

	return job, nil
}
