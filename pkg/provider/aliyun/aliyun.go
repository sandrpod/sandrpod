// Copyright 2024 SandrPod
// Alibaba Cloud provider implementation

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

// AliyunProvider is the Alibaba Cloud implementation of the Provider interface.
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

// Config holds Alibaba Cloud credentials and region settings.
type Config struct {
	Region    string // Region, e.g. "cn-hangzhou"
	AccessKey string // Access Key ID
	SecretKey string // Access Key Secret
}

// NewAliyunProvider creates an Alibaba Cloud provider from the given configuration.
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
	return "Alibaba Cloud"
}

// mapInstanceState maps an Alibaba Cloud ECS instance status string to VMState.
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

// mapEcsInstanceToVM maps an ECS instance to a VMInfo struct.
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

// CreateVM creates an ECS instance on Alibaba Cloud.
func (p *AliyunProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	// Resolve image ID
	imageID := req.ImageID
	if imageID == "" {
		var err error
		imageID, err = p.GetDefaultImage(ctx, req.Region)
		if err != nil {
			imageID = "ubuntu_22_04_64_20G_alibase_20230920.vhd" // default Ubuntu image
		}
	}

	// Build the create instance request
	createReq := ecs.CreateCreateInstanceRequest()
	createReq.RegionId = req.Region
	createReq.ImageId = imageID
	createReq.InstanceType = req.InstanceType
	createReq.InstanceName = req.Name

	// Security group
	if req.NetworkConfig != nil && req.NetworkConfig.SecurityGroup != "" {
		createReq.SecurityGroupId = req.NetworkConfig.SecurityGroup
	}

	// VPC configuration - using VSwitchId
	if req.NetworkConfig != nil && req.NetworkConfig.VpcID != "" {
		if req.NetworkConfig.SubnetID != "" {
			createReq.VSwitchId = req.NetworkConfig.SubnetID
		}
	}

	// Public IP
	if req.NetworkConfig != nil && req.NetworkConfig.PublicIP {
		createReq.InternetMaxBandwidthOut = requests.NewInteger(10) // 10 Mbps
	}

	// Disk configuration
	if req.DiskConfig != nil {
		if req.DiskConfig.SizeGiB > 0 {
			createReq.SystemDiskSize = requests.NewInteger(req.DiskConfig.SizeGiB)
			createReq.SystemDiskCategory = req.DiskConfig.VolumeType
		}
	}

	// Create the instance
	resp, err := p.ecsClient.CreateInstance(createReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create instance: %w", err)
	}

	instanceID := resp.InstanceId

	// Start the instance
	startReq := ecs.CreateStartInstanceRequest()
	startReq.InstanceId = instanceID
	_, err = p.ecsClient.StartInstance(startReq)
	if err != nil {
		// If start fails, clean up the instance
		delReq := ecs.CreateDeleteInstanceRequest()
		delReq.InstanceId = instanceID
		delReq.Force = "true"
		p.ecsClient.DeleteInstance(delReq)
		return nil, fmt.Errorf("failed to start instance: %w", err)
	}

	// Build VMInfo
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

// DeleteVM terminates and removes the specified ECS instance.
func (p *AliyunProvider) DeleteVM(ctx context.Context, vmID string) error {
	req := ecs.CreateDeleteInstanceRequest()
	req.InstanceId = vmID
	req.Force = "true" // Force delete even if running

	_, err := p.ecsClient.DeleteInstance(req)
	if err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	p.mu.Lock()
	delete(p.vms, vmID)
	p.mu.Unlock()

	return nil
}

// GetVM retrieves information about a VM instance, checking the local cache first.
func (p *AliyunProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	// Check local cache first
	p.mu.RLock()
	if vm, ok := p.vms[vmID]; ok {
		p.mu.RUnlock()
		return vm, nil
	}
	p.mu.RUnlock()

	// Query Alibaba Cloud ECS
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

// ListVMs lists all SandrPod-managed ECS instances in the configured region.
func (p *AliyunProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	req := ecs.CreateDescribeInstancesRequest()
	req.RegionId = p.region

	// Filter to only instances created by SandrPod (via tag)
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

// ExecuteCommand runs a shell command on the instance via the Alibaba Cloud CloudAssist service.
// Note: CloudAssist requires the CloudAssist client to be installed on the instance.
// This implementation simplifies the CloudAssist flow; refer to the Alibaba Cloud SDK docs for production usage.
func (p *AliyunProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	// Create a CloudAssist command
	cmdReq := ecs.CreateCreateCommandRequest()
	cmdReq.RegionId = p.region
	cmdReq.Type = "RunShellScript"
	cmdReq.CommandContent = command
	cmdReq.Name = fmt.Sprintf("sandrpod-%d", time.Now().Unix())

	cmdResp, err := p.ecsClient.CreateCommand(cmdReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create command: %w", err)
	}

	// Invoke the command
	execReq := ecs.CreateInvokeCommandRequest()
	execReq.InstanceId = &[]string{vmID}
	execReq.CommandId = cmdResp.CommandId

	execResp, err := p.ecsClient.InvokeCommand(execReq)
	if err != nil {
		return nil, fmt.Errorf("failed to invoke command: %w", err)
	}

	// Poll for the execution result
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range 30 { // wait up to 60 seconds
		<-ticker.C

		// Query invocation results
		outputReq := ecs.CreateDescribeInvocationResultsRequest()
		outputReq.InstanceId = vmID
		outputReq.InvokeId = execResp.InvokeId

		outputResp, err := p.ecsClient.DescribeInvocationResults(outputReq)
		if err != nil {
			continue
		}

		// Check if results are available - InvocationResults is nested under outputResp.Invocation.InvocationResults
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

// WaitUntilRunning blocks until the VM reaches the Running state or the timeout expires.
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

// GetHealthStatus returns the health status of a VM and its services.
func (p *AliyunProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}

	status := &provider.HealthStatus{
		VMReady: vm.State == provider.VMStateRunning,
	}

	// Use CloudAssist to verify Docker is running
	if vm.State == provider.VMStateRunning && vm.PublicIP != "" {
		checkCmd := "docker ps > /dev/null 2>&1 && echo 'ok' || echo 'fail'"
		result, err := p.ExecuteCommand(ctx, vmID, checkCmd)
		if err == nil && result.ExitCode == 0 {
			status.DockerReady = true
		}
	}

	return status, nil
}

// ListRegions returns all available Alibaba Cloud regions.
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

// ListInstanceTypes returns all available ECS instance types for the given region.
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

// GetDefaultImage returns the default system image ID for the given region.
func (p *AliyunProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	// Check the image cache first
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

	// Fall back to a default Ubuntu image
	return "ubuntu_22_04_64_20G_alibase_20230920.vhd", nil
}

// Cleanup removes all SandrPod-managed VMs.
func (p *AliyunProvider) Cleanup(ctx context.Context) error {
	vms, err := p.ListVMs(ctx)
	if err != nil {
		return err
	}

	for _, vm := range vms {
		if err := p.DeleteVM(ctx, vm.ID); err != nil {
			// Log the error but continue cleaning up remaining VMs
			fmt.Printf("failed to delete VM %s: %v\n", vm.ID, err)
		}
	}

	return nil
}
