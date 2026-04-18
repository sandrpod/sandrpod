// Copyright 2024 SandrPod
// AWS Provider 实现

package aws

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// AWSProvider AWS Provider
type AWSProvider struct {
	region       string
	accessKey    string
	secretKey    string
	ec2Client    *ec2.Client
	ssmClient    *ssm.Client
	mu           sync.RWMutex
	vms          map[string]*provider.VMInfo
	instanceCache map[string]string
}

// Config AWS 配置
type Config struct {
	Region    string // 区域，如 "us-east-1"
	AccessKey string // Access Key ID
	SecretKey string // Access Key Secret
}

// NewAWSProvider 创建 AWS Provider
func NewAWSProvider(cfg *Config) (*AWSProvider, error) {
	var awsCfg aws.Config
	var err error

	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		// 使用凭证
		awsCfg, err = config.LoadDefaultConfig(context.Background(),
			config.WithRegion(cfg.Region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				cfg.AccessKey, cfg.SecretKey, "")),
		)
	} else {
		// 使用默认凭证链 (IAM Role, 环境变量等)
		awsCfg, err = config.LoadDefaultConfig(context.Background(),
			config.WithRegion(cfg.Region),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &AWSProvider{
		region:       cfg.Region,
		accessKey:    cfg.AccessKey,
		secretKey:    cfg.SecretKey,
		ec2Client:    ec2.NewFromConfig(awsCfg),
		ssmClient:    ssm.NewFromConfig(awsCfg),
		vms:          make(map[string]*provider.VMInfo),
		instanceCache: make(map[string]string),
	}, nil
}

func (p *AWSProvider) Name() string {
	return "aws"
}

func (p *AWSProvider) DisplayName() string {
	return "Amazon Web Services"
}

// mapInstanceState 映射 EC2 实例状态
func mapInstanceState(state string) provider.VMState {
	switch state {
	case "running":
		return provider.VMStateRunning
	case "pending":
		return provider.VMStatePending
	case "shutting-down", "stopping":
		return provider.VMStateStopping
	case "stopped", "terminated":
		return provider.VMStateStopped
	default:
		return provider.VMStatePending
	}
}

// mapEC2ToVMInfo 映射 EC2 实例到 VMInfo
func mapEC2ToVMInfo(instance types.Instance) *provider.VMInfo {
	publicIP := ""
	if instance.PublicIpAddress != nil {
		publicIP = *instance.PublicIpAddress
	}

	privateIP := ""
	if instance.PrivateIpAddress != nil {
		privateIP = *instance.PrivateIpAddress
	}

	state := string(instance.State.Name)

	name := ""
	for _, tag := range instance.Tags {
		if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
			name = *tag.Value
			break
		}
	}

	return &provider.VMInfo{
		ID:           *instance.InstanceId,
		Name:         name,
		Region:       *instance.Placement.AvailabilityZone,
		InstanceType: string(instance.InstanceType),
		State:        mapInstanceState(state),
		PublicIP:     publicIP,
		PrivateIP:    privateIP,
		CreatedAt:    time.Time{},
	}
}

// CreateVM 创建 VM
func (p *AWSProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	// 获取镜像 ID
	imageID := req.ImageID
	if imageID == "" {
		var err error
		imageID, err = p.GetDefaultImage(ctx, req.Region)
		if err != nil {
			imageID = "ami-0c55b159cbfafe1f0" // 默认 Ubuntu 22.04
		}
	}

	// 构建 Tag 列表
	tags := []types.Tag{
		{Key: aws.String("sandrpod"), Value: aws.String("true")},
		{Key: aws.String("Name"), Value: aws.String(req.Name)},
	}
	for k, v := range req.Tags {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}

	// 创建 EC2 实例
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String(imageID),
		InstanceType: types.InstanceType(req.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         tags,
			},
		},
	}

	// VPC 配置
	if req.NetworkConfig != nil {
		if req.NetworkConfig.VpcID != "" {
			input.SubnetId = aws.String(req.NetworkConfig.SubnetID)
		}
		if req.NetworkConfig.SecurityGroup != "" {
			input.SecurityGroupIds = []string{req.NetworkConfig.SecurityGroup}
		}
	}

	// 公网 IP - 通过 NetworkInterfaces 配置
	if req.NetworkConfig != nil && req.NetworkConfig.PublicIP {
		input.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{
			{
				AssociatePublicIpAddress: aws.Bool(true),
				DeviceIndex:             aws.Int32(0),
			},
		}
	}

	// 磁盘配置
	if req.DiskConfig != nil && req.DiskConfig.SizeGiB > 0 {
		input.BlockDeviceMappings = []types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &types.EbsBlockDevice{
					VolumeSize: aws.Int32(int32(req.DiskConfig.SizeGiB)),
					VolumeType: types.VolumeType(req.DiskConfig.VolumeType),
					Encrypted:  aws.Bool(req.DiskConfig.Encrypted),
				},
			},
		}
	}

	resp, err := p.ec2Client.RunInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to create instance: %w", err)
	}

	if len(resp.Instances) == 0 {
		return nil, fmt.Errorf("no instance created")
	}

	instance := resp.Instances[0]
	instanceID := *instance.InstanceId

	// 等待实例运行
	waiter := ec2.NewInstanceRunningWaiter(p.ec2Client)
	err = waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, 5*time.Minute)
	if err != nil {
		// 即使等待失败，实例可能已经创建
		fmt.Printf("warning: instance %s may not be running yet: %v\n", instanceID, err)
	}

	vmInfo := &provider.VMInfo{
		ID:           instanceID,
		Name:         req.Name,
		Region:       p.region,
		InstanceType: req.InstanceType,
		State:        provider.VMStatePending,
	}

	p.mu.Lock()
	p.vms[instanceID] = vmInfo
	p.mu.Unlock()

	return vmInfo, nil
}

// DeleteVM 删除 VM
func (p *AWSProvider) DeleteVM(ctx context.Context, vmID string) error {
	_, err := p.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{vmID},
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instance: %w", err)
	}

	p.mu.Lock()
	delete(p.vms, vmID)
	p.mu.Unlock()

	return nil
}

// GetVM 获取 VM
func (p *AWSProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	// 先查本地缓存
	p.mu.RLock()
	if vm, ok := p.vms[vmID]; ok {
		p.mu.RUnlock()
		return vm, nil
	}
	p.mu.RUnlock()

	// 查询 EC2
	resp, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{vmID},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("instance %s not found", vmID)
	}

	return mapEC2ToVMInfo(resp.Reservations[0].Instances[0]), nil
}

// ListVMs 列出所有 VM
func (p *AWSProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	resp, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:sandrpod"), Values: []string{"true"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %w", err)
	}

	vms := make([]*provider.VMInfo, 0)
	for _, reservation := range resp.Reservations {
		for _, instance := range reservation.Instances {
			vms = append(vms, mapEC2ToVMInfo(instance))
		}
	}

	return vms, nil
}

// ExecuteCommand 执行命令 (通过 SSM)
func (p *AWSProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	// 使用 SSM SendCommand
	input := &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{vmID},
		Parameters: map[string][]string{
			"commands": {command},
		},
	}

	cmdResp, err := p.ssmClient.SendCommand(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to send command: %w", err)
	}

	commandID := *cmdResp.Command.CommandId

	// 等待命令执行
	describeInput := &ssm.ListCommandInvocationsInput{
		CommandId:  aws.String(commandID),
		InstanceId: aws.String(vmID),
	}

	var output string
	var exitCode int

	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)

		resp, err := p.ssmClient.ListCommandInvocations(ctx, describeInput)
		if err != nil {
			continue
		}

		if len(resp.CommandInvocations) > 0 {
			invocation := resp.CommandInvocations[0]
			if invocation.Status != "Pending" && invocation.Status != "InProgress" {
				// 获取输出
				outputResp, _ := p.ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
					CommandId: aws.String(commandID),
					InstanceId: aws.String(vmID),
				})
				if outputResp != nil {
					output = *outputResp.StandardOutputContent
				}
				if invocation.Status == "Success" {
					exitCode = 0
				} else {
					exitCode = 1
				}
				break
			}
		}
	}

	return &provider.CommandResult{
		Output:     strings.TrimSpace(output),
		ExitCode:   exitCode,
		Stderr:     "",
		ExecutedAt: time.Now(),
	}, nil
}

// WaitUntilRunning 等待 VM 运行
func (p *AWSProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	waiter := ec2.NewInstanceRunningWaiter(p.ec2Client)
	return waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{vmID},
	}, timeout)
}

// GetHealthStatus 获取健康状态
func (p *AWSProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}

	status := &provider.HealthStatus{
		VMReady: vm.State == provider.VMStateRunning,
	}

	// 通过 SSM 检查 Docker 是否运行
	if vm.State == provider.VMStateRunning {
		checkCmd := "docker ps > /dev/null 2>&1 && echo 'ok' || echo 'fail'"
		result, err := p.ExecuteCommand(ctx, vmID, checkCmd)
		if err == nil && result.ExitCode == 0 {
			status.DockerReady = true
		}
	}

	return status, nil
}

// ListRegions 列出可用区域
func (p *AWSProvider) ListRegions(ctx context.Context) ([]string, error) {
	resp, err := p.ec2Client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to describe regions: %w", err)
	}

	regions := make([]string, 0, len(resp.Regions))
	for _, r := range resp.Regions {
		regions = append(regions, *r.RegionName)
	}

	return regions, nil
}

// ListInstanceTypes 列出实例类型
func (p *AWSProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	// 常用的实例类型
	commonTypes := []*provider.InstanceType{
		{Name: "t3.micro", CPU: 2, MemoryGiB: 1, GPU: 0},
		{Name: "t3.small", CPU: 2, MemoryGiB: 2, GPU: 0},
		{Name: "t3.medium", CPU: 2, MemoryGiB: 4, GPU: 0},
		{Name: "t3.large", CPU: 2, MemoryGiB: 8, GPU: 0},
		{Name: "m5.large", CPU: 2, MemoryGiB: 8, GPU: 0},
		{Name: "m5.xlarge", CPU: 4, MemoryGiB: 16, GPU: 0},
		{Name: "m5.2xlarge", CPU: 8, MemoryGiB: 32, GPU: 0},
		{Name: "c5.large", CPU: 2, MemoryGiB: 4, GPU: 0},
		{Name: "c5.xlarge", CPU: 4, MemoryGiB: 8, GPU: 0},
		{Name: "c5.2xlarge", CPU: 8, MemoryGiB: 16, GPU: 0},
		{Name: "r5.large", CPU: 2, MemoryGiB: 16, GPU: 0},
		{Name: "r5.xlarge", CPU: 4, MemoryGiB: 32, GPU: 0},
		{Name: "g4dn.xlarge", CPU: 4, MemoryGiB: 16, GPU: 1, GPUType: "NVIDIA T4"},
		{Name: "g4dn.2xlarge", CPU: 8, MemoryGiB: 32, GPU: 1, GPUType: "NVIDIA T4"},
	}

	return commonTypes, nil
}

// GetDefaultImage 获取默认镜像
func (p *AWSProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	// 查询最新的 Ubuntu 22.04 LTS AMI
	resp, err := p.ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: []types.Filter{
			{Name: aws.String("name"), Values: []string{"ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-*"}},
			{Name: aws.String("architecture"), Values: []string{"x86_64"}},
			{Name: aws.String("state"), Values: []string{"available"}},
			{Name: aws.String("image-type"), Values: []string{"machine"}},
		},
		Owners: []string{"099720109477"}, // Canonical
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe images: %w", err)
	}

	if len(resp.Images) > 0 {
		// 返回最新的镜像
		return *resp.Images[0].ImageId, nil
	}

	// 返回默认镜像
	return "ami-0c55b159cbfafe1f0", nil
}

// Cleanup 清理资源
func (p *AWSProvider) Cleanup(ctx context.Context) error {
	vms, err := p.ListVMs(ctx)
	if err != nil {
		return err
	}

	for _, vm := range vms {
		if err := p.DeleteVM(ctx, vm.ID); err != nil {
			fmt.Printf("failed to delete VM %s: %v\n", vm.ID, err)
		}
	}

	return nil
}
