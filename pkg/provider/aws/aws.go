// Copyright 2024 SandrPod
// AWS Provider implementation

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

// Config AWS configuration
type Config struct {
	Region    string // Region, e.g. "us-east-1"
	AccessKey string // Access Key ID
	SecretKey string // Access Key Secret
}

// NewAWSProvider creates a new AWS Provider
func NewAWSProvider(cfg *Config) (*AWSProvider, error) {
	var awsCfg aws.Config
	var err error

	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		// Use explicit credentials
		awsCfg, err = config.LoadDefaultConfig(context.Background(),
			config.WithRegion(cfg.Region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				cfg.AccessKey, cfg.SecretKey, "")),
		)
	} else {
		// Use the default credential chain (IAM Role, environment variables, etc.)
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

// mapInstanceState maps an EC2 instance state string to a VMState
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

// mapEC2ToVMInfo converts an EC2 instance to a VMInfo struct
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

// CreateVM creates a new EC2 VM instance
func (p *AWSProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	// Resolve image ID
	imageID := req.ImageID
	if imageID == "" {
		var err error
		imageID, err = p.GetDefaultImage(ctx, req.Region)
		if err != nil {
			imageID = "ami-0c55b159cbfafe1f0" // Default Ubuntu 22.04
		}
	}

	// Build tag list
	tags := []types.Tag{
		{Key: aws.String("sandrpod"), Value: aws.String("true")},
		{Key: aws.String("Name"), Value: aws.String(req.Name)},
	}
	for k, v := range req.Tags {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}

	// Launch EC2 instance
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

	// VPC configuration
	if req.NetworkConfig != nil {
		if req.NetworkConfig.VpcID != "" {
			input.SubnetId = aws.String(req.NetworkConfig.SubnetID)
		}
		if req.NetworkConfig.SecurityGroup != "" {
			input.SecurityGroupIds = []string{req.NetworkConfig.SecurityGroup}
		}
	}

	// Public IP - configured via NetworkInterfaces
	if req.NetworkConfig != nil && req.NetworkConfig.PublicIP {
		input.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{
			{
				AssociatePublicIpAddress: aws.Bool(true),
				DeviceIndex:             aws.Int32(0),
			},
		}
	}

	// Disk configuration
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

	// Wait for the instance to reach running state
	waiter := ec2.NewInstanceRunningWaiter(p.ec2Client)
	err = waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, 5*time.Minute)
	if err != nil {
		// The instance may have been created even if the waiter timed out
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

// DeleteVM terminates an EC2 instance
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

// GetVM retrieves a VM by ID
func (p *AWSProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	// Check local cache first
	p.mu.RLock()
	if vm, ok := p.vms[vmID]; ok {
		p.mu.RUnlock()
		return vm, nil
	}
	p.mu.RUnlock()

	// Query EC2
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

// ListVMs returns all VMs tagged with sandrpod
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

// ExecuteCommand runs a shell command on a VM via SSM
func (p *AWSProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	// Use SSM SendCommand
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

	// Poll until the command finishes
	describeInput := &ssm.ListCommandInvocationsInput{
		CommandId:  aws.String(commandID),
		InstanceId: aws.String(vmID),
	}

	var output string
	var exitCode int

	for range 30 {
		time.Sleep(2 * time.Second)

		resp, err := p.ssmClient.ListCommandInvocations(ctx, describeInput)
		if err != nil {
			continue
		}

		if len(resp.CommandInvocations) > 0 {
			invocation := resp.CommandInvocations[0]
			if invocation.Status != "Pending" && invocation.Status != "InProgress" {
				// Retrieve command output
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

// WaitUntilRunning blocks until the VM reaches the running state
func (p *AWSProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	waiter := ec2.NewInstanceRunningWaiter(p.ec2Client)
	return waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{vmID},
	}, timeout)
}

// GetHealthStatus returns the health status of a VM
func (p *AWSProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}

	status := &provider.HealthStatus{
		VMReady: vm.State == provider.VMStateRunning,
	}

	// Check whether Docker is running via SSM
	if vm.State == provider.VMStateRunning {
		checkCmd := "docker ps > /dev/null 2>&1 && echo 'ok' || echo 'fail'"
		result, err := p.ExecuteCommand(ctx, vmID, checkCmd)
		if err == nil && result.ExitCode == 0 {
			status.DockerReady = true
		}
	}

	return status, nil
}

// ListRegions returns all available AWS regions
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

// ListInstanceTypes returns commonly used EC2 instance types
func (p *AWSProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	// Commonly used instance types
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

// GetDefaultImage returns the latest Ubuntu 22.04 LTS AMI ID for the region
func (p *AWSProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	// Look up the latest Ubuntu 22.04 LTS AMI
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
		// Return the most recent image
		return *resp.Images[0].ImageId, nil
	}

	// Fall back to a known default image
	return "ami-0c55b159cbfafe1f0", nil
}

// Cleanup removes all VMs managed by this provider
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
