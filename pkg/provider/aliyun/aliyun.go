// Copyright 2024 SandrPod
// 阿里云 Provider 实现

package aliyun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// AliyunProvider 阿里云 Provider
type AliyunProvider struct {
	region      string
	accessKey   string
	secretKey   string
	ecsClient   *ecs.Client
	mu          sync.RWMutex
	vms         map[string]*provider.VMInfo
	imageCache  map[string]string
	instanceCache map[string]string // instance type -> id mapping
}

// Config 阿里云配置
type Config struct {
	Region    string // 区域，如 "cn-hangzhou"
	AccessKey string // Access Key ID
	SecretKey string // Access Key Secret
}

// NewAliyunProvider 创建阿里云 Provider
func NewAliyunProvider(cfg *Config) (*AliyunProvider, error) {
	client, err := ecs.NewClientWithAccessKey(cfg.Region, cfg.AccessKey, cfg.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create ECS client: %w", err)
	}

	return &AliyunProvider{
		region:       cfg.Region,
		accessKey:    cfg.AccessKey,
		secretKey:    cfg.SecretKey,
		ecsClient:    client,
		vms:          make(map[string]*provider.VMInfo),
		imageCache:   make(map[string]string),
		instanceCache: make(map[string]string),
	}, nil
}

func (p *AliyunProvider) Name() string {
	return "aliyun"
}

func (p *AliyunProvider) DisplayName() string {
	return "阿里云"
}

// mapInstanceState 映射阿里云实例状态到 VMState
func mapInstanceState(ecsState string) provider.VMState {
	switch ecsState {
	case "Running":
		return provider.VMStateRunning
	case "Starting":
		return provider.VMStatePending
	case "Stopping":
		return provider.VMStateStopping
	case "Stopped":
		return provider.VMStateStopped
	default:
		return provider.VMStatePending
	}
}

// mapEcsInstanceToVM 映射 ECS 实例到 VMInfo
func mapEcsInstanceToVM(instance ecs.Instance) *provider.VMInfo {
	publicIP := ""
	if len(instance.PublicIpAddress.IpAddress) > 0 {
		publicIP = instance.PublicIpAddress.IpAddress[0]
	}

	privateIP := ""
	if len(instance.InnerIpAddress.IpAddress) > 0 {
		privateIP = instance.InnerIpAddress.IpAddress[0]
	}

	return &provider.VMInfo{
		ID:           instance.InstanceId,
		Name:         instance.InstanceName,
		Region:       instance.RegionId,
		InstanceType: instance.InstanceType,
		State:        mapInstanceState(instance.Status),
		PublicIP:     publicIP,
		PrivateIP:    privateIP,
		CreatedAt:    time.Time{},
	}
}

// CreateVM 创建 VM
func (p *AliyunProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	// 获取镜像 ID
	imageID := req.ImageID
	if imageID == "" {
		var err error
		imageID, err = p.GetDefaultImage(ctx, req.Region)
		if err != nil {
			imageID = "ubuntu_22_04_64_20G_alibase_20230920.vhd" // 默认 Ubuntu
		}
	}

	// 创建实例请求
	createReq := ecs.CreateCreateInstanceRequest()
	createReq.RegionId = req.Region
	createReq.ImageId = imageID
	createReq.InstanceType = req.InstanceType
	createReq.InstanceName = req.Name

	// 安全组
	if req.NetworkConfig != nil && req.NetworkConfig.SecurityGroup != "" {
		createReq.SecurityGroupId = req.NetworkConfig.SecurityGroup
	}

	// VPC 配置 - 使用 VSwitchId 方式
	if req.NetworkConfig != nil && req.NetworkConfig.VpcID != "" {
		if req.NetworkConfig.SubnetID != "" {
			createReq.VSwitchId = req.NetworkConfig.SubnetID
		}
	}

	// 公网 IP
	if req.NetworkConfig != nil && req.NetworkConfig.PublicIP {
		createReq.InternetMaxBandwidthOut = requests.NewInteger(10) // 10Mbps
	}

	// 磁盘配置
	if req.DiskConfig != nil {
		if req.DiskConfig.SizeGiB > 0 {
			createReq.SystemDiskSize = requests.NewInteger(req.DiskConfig.SizeGiB)
			createReq.SystemDiskCategory = req.DiskConfig.VolumeType
		}
	}

	// 创建实例
	resp, err := p.ecsClient.CreateInstance(createReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create instance: %w", err)
	}

	instanceID := resp.InstanceId

	// 启动实例
	startReq := ecs.CreateStartInstanceRequest()
	startReq.InstanceId = instanceID
	_, err = p.ecsClient.StartInstance(startReq)
	if err != nil {
		// 如果启动失败，删除实例
		delReq := ecs.CreateDeleteInstanceRequest()
		delReq.InstanceId = instanceID
		delReq.Force = "true"
		p.ecsClient.DeleteInstance(delReq)
		return nil, fmt.Errorf("failed to start instance: %w", err)
	}

	// 构建 VMInfo
	vmInfo := &provider.VMInfo{
		ID:           instanceID,
		Name:         req.Name,
		Region:       req.Region,
		InstanceType: req.InstanceType,
		State:        provider.VMStatePending,
	}

	p.mu.Lock()
	p.vms[instanceID] = vmInfo
	p.mu.Unlock()

	return vmInfo, nil
}

// DeleteVM 删除 VM
func (p *AliyunProvider) DeleteVM(ctx context.Context, vmID string) error {
	req := ecs.CreateDeleteInstanceRequest()
	req.InstanceId = vmID
	req.Force = "true" // 强制删除

	_, err := p.ecsClient.DeleteInstance(req)
	if err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	p.mu.Lock()
	delete(p.vms, vmID)
	p.mu.Unlock()

	return nil
}

// GetVM 获取 VM
func (p *AliyunProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	// 先查本地缓存
	p.mu.RLock()
	if vm, ok := p.vms[vmID]; ok {
		p.mu.RUnlock()
		return vm, nil
	}
	p.mu.RUnlock()

	// 查询阿里云
	req := ecs.CreateDescribeInstancesRequest()
	req.InstanceIds = fmt.Sprintf(`["%s"]`, vmID)

	resp, err := p.ecsClient.DescribeInstances(req)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(resp.Instances.Instance) == 0 {
		return nil, fmt.Errorf("instance %s not found", vmID)
	}

	return mapEcsInstanceToVM(resp.Instances.Instance[0]), nil
}

// ListVMs 列出所有 VM
func (p *AliyunProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	req := ecs.CreateDescribeInstancesRequest()
	req.RegionId = p.region

	// 只查询 SandrPod 创建的实例 (通过标签筛选)
	req.Tag = &[]ecs.DescribeInstancesTag{
		{Key: "sandrpod", Value: "true"},
	}

	resp, err := p.ecsClient.DescribeInstances(req)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %w", err)
	}

	vms := make([]*provider.VMInfo, 0, len(resp.Instances.Instance))
	for _, inst := range resp.Instances.Instance {
		vms = append(vms, mapEcsInstanceToVM(inst))
	}

	return vms, nil
}

// ExecuteCommand 执行命令 (通过云助手)
// 注意：阿里云云助手需要实例已安装云助手客户端
// 这个实现简化了云助手功能，实际使用需要根据阿里云 SDK 文档调整
func (p *AliyunProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	// 创建云助手命令
	cmdReq := ecs.CreateCreateCommandRequest()
	cmdReq.RegionId = p.region
	cmdReq.Type = "RunShellScript"
	cmdReq.CommandContent = command
	cmdReq.Name = fmt.Sprintf("sandrpod-%d", time.Now().Unix())

	cmdResp, err := p.ecsClient.CreateCommand(cmdReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create command: %w", err)
	}

	// 执行命令
	execReq := ecs.CreateInvokeCommandRequest()
	execReq.InstanceId = &[]string{vmID}
	execReq.CommandId = cmdResp.CommandId

	execResp, err := p.ecsClient.InvokeCommand(execReq)
	if err != nil {
		return nil, fmt.Errorf("failed to invoke command: %w", err)
	}

	// 等待执行结果
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for i := 0; i < 30; i++ { // 最多等待 60 秒
		<-ticker.C

		// 查询执行结果
		outputReq := ecs.CreateDescribeInvocationResultsRequest()
		outputReq.InstanceId = vmID
		outputReq.InvokeId = execResp.InvokeId

		outputResp, err := p.ecsClient.DescribeInvocationResults(outputReq)
		if err != nil {
			continue
		}

		// 检查是否有结果 - InvocationResults 在 outputResp.Invocation.InvocationResults 中
		if outputResp != nil && outputResp.Invocation.InvocationResults.InvocationResult != nil && len(outputResp.Invocation.InvocationResults.InvocationResult) > 0 {
			result := outputResp.Invocation.InvocationResults.InvocationResult[0]
			return &provider.CommandResult{
				Output:     result.Output,
				ExitCode:  int(result.ExitCode),
				Stderr:    "",
				ExecutedAt: time.Now(),
			}, nil
		}
	}

	return nil, fmt.Errorf("command execution timeout")
}

// WaitUntilRunning 等待 VM 运行
func (p *AliyunProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for VM %s to be running", vmID)
		}

		vm, err := p.GetVM(ctx, vmID)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				continue
			}
		}

		if vm.State == provider.VMStateRunning {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// GetHealthStatus 获取健康状态
func (p *AliyunProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}

	status := &provider.HealthStatus{
		VMReady: vm.State == provider.VMStateRunning,
	}

	// 通过云助手检查 Docker 是否运行
	if vm.State == provider.VMStateRunning && vm.PublicIP != "" {
		checkCmd := "docker ps > /dev/null 2>&1 && echo 'ok' || echo 'fail'"
		result, err := p.ExecuteCommand(ctx, vmID, checkCmd)
		if err == nil && result.ExitCode == 0 {
			status.DockerReady = true
		}
	}

	return status, nil
}

// ListRegions 列出可用区域
func (p *AliyunProvider) ListRegions(ctx context.Context) ([]string, error) {
	req := ecs.CreateDescribeRegionsRequest()

	resp, err := p.ecsClient.DescribeRegions(req)
	if err != nil {
		return nil, fmt.Errorf("failed to describe regions: %w", err)
	}

	regions := make([]string, 0, len(resp.Regions.Region))
	for _, r := range resp.Regions.Region {
		regions = append(regions, r.RegionId)
	}

	return regions, nil
}

// ListInstanceTypes 列出实例类型
func (p *AliyunProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	req := ecs.CreateDescribeInstanceTypesRequest()

	resp, err := p.ecsClient.DescribeInstanceTypes(req)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instance types: %w", err)
	}

	types := make([]*provider.InstanceType, 0, len(resp.InstanceTypes.InstanceType))
	for _, it := range resp.InstanceTypes.InstanceType {
		types = append(types, &provider.InstanceType{
			Name:      it.InstanceTypeId,
			CPU:       float64(it.CpuCoreCount),
			MemoryGiB: it.MemorySize,
			DiskGiB:   0,
			GPU:       0,
			GPUType:   "",
		})
	}

	return types, nil
}

// GetDefaultImage 获取默认镜像
func (p *AliyunProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	// 缓存检查
	if img, ok := p.imageCache[region]; ok {
		return img, nil
	}

	req := ecs.CreateDescribeImagesRequest()
	req.RegionId = region
	req.ImageOwnerAlias = "system"

	resp, err := p.ecsClient.DescribeImages(req)
	if err != nil {
		return "", fmt.Errorf("failed to describe images: %w", err)
	}

	if len(resp.Images.Image) > 0 {
		imageID := resp.Images.Image[0].ImageId
		p.imageCache[region] = imageID
		return imageID, nil
	}

	// 返回默认 Ubuntu 镜像
	return "ubuntu_22_04_64_20G_alibase_20230920.vhd", nil
}

// Cleanup 清理资源
func (p *AliyunProvider) Cleanup(ctx context.Context) error {
	vms, err := p.ListVMs(ctx)
	if err != nil {
		return err
	}

	for _, vm := range vms {
		if err := p.DeleteVM(ctx, vm.ID); err != nil {
			// 记录错误但继续清理
			fmt.Printf("failed to delete VM %s: %v\n", vm.ID, err)
		}
	}

	return nil
}
