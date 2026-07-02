// Copyright 2024 SandrPod
// AWS Provider implementation

package aws

import (
	"context"
	"errors"
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
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// AWSProvider AWS Provider
type AWSProvider struct {
	region    string
	accessKey string
	secretKey string
	// iamInstanceProfile is attached to launched instances so they become
	// SSM-managed (required for ExecuteCommand). Empty disables it.
	iamInstanceProfile string
	ec2Client          *ec2.Client
	ssmClient          *ssm.Client
	mu                 sync.RWMutex
	vms                map[string]*provider.VMInfo
}

// Config AWS configuration
type Config struct {
	Region    string // Region, e.g. "us-east-1"
	AccessKey string // Access Key ID
	SecretKey string // Access Key Secret
	// IAMInstanceProfile is the instance-profile name attached to new VMs so
	// SSM can run commands on them. Without it, ExecuteCommand cannot work.
	IAMInstanceProfile string
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
		region:             cfg.Region,
		accessKey:          cfg.AccessKey,
		secretKey:          cfg.SecretKey,
		iamInstanceProfile: cfg.IAMInstanceProfile,
		ec2Client:          ec2.NewFromConfig(awsCfg),
		ssmClient:          ssm.NewFromConfig(awsCfg),
		vms:                make(map[string]*provider.VMInfo),
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

	createdAt := time.Time{}
	if instance.LaunchTime != nil {
		createdAt = *instance.LaunchTime
	}

	return &provider.VMInfo{
		ID:           *instance.InstanceId,
		Name:         name,
		Region:       *instance.Placement.AvailabilityZone,
		InstanceType: string(instance.InstanceType),
		State:        mapInstanceState(state),
		PublicIP:     publicIP,
		PrivateIP:    privateIP,
		CreatedAt:    createdAt,
	}
}

// CreateVM creates a new EC2 VM instance
func (p *AWSProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	// Resolve image ID. Fail loudly rather than falling back to a hard-coded
	// AMI that is region-specific and likely deregistered.
	imageID := req.ImageID
	if imageID == "" {
		resolved, err := p.GetDefaultImage(ctx, req.Region)
		if err != nil {
			return nil, fmt.Errorf("no image specified and default image lookup failed: %w", err)
		}
		imageID = resolved
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

	// IAM instance profile so the instance becomes SSM-managed (required by
	// ExecuteCommand). Without it, SSM SendCommand has no target to run on.
	if p.iamInstanceProfile != "" {
		input.IamInstanceProfile = &types.IamInstanceProfileSpecification{
			Name: aws.String(p.iamInstanceProfile),
		}
	}

	// Network configuration. A requested public IP MUST be set on a network
	// interface, and AWS forbids combining top-level SubnetId/SecurityGroupIds
	// with NetworkInterfaces — so in that case the subnet and security group
	// move INTO the interface.
	if nc := req.NetworkConfig; nc != nil {
		if nc.PublicIP {
			ni := types.InstanceNetworkInterfaceSpecification{
				AssociatePublicIpAddress: aws.Bool(true),
				DeviceIndex:              aws.Int32(0),
			}
			if nc.SubnetID != "" {
				ni.SubnetId = aws.String(nc.SubnetID)
			}
			if nc.SecurityGroup != "" {
				ni.Groups = []string{nc.SecurityGroup}
			}
			input.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{ni}
		} else {
			if nc.SubnetID != "" {
				input.SubnetId = aws.String(nc.SubnetID)
			}
			if nc.SecurityGroup != "" {
				input.SecurityGroupIds = []string{nc.SecurityGroup}
			}
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

	// Wait for the instance to reach running state.
	waiter := ec2.NewInstanceRunningWaiter(p.ec2Client)
	waitErr := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, 5*time.Minute)

	// Re-describe to capture the assigned public/private IPs and the real
	// state — the scheduler needs PublicIP to bootstrap Poder, so a bare
	// instance ID with a hard-coded Pending state isn't enough.
	vmInfo := &provider.VMInfo{
		ID:           instanceID,
		Name:         req.Name,
		Region:       p.region,
		InstanceType: req.InstanceType,
		State:        provider.VMStatePending,
	}
	desc, derr := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if derr == nil && len(desc.Reservations) > 0 && len(desc.Reservations[0].Instances) > 0 {
		vmInfo = mapEC2ToVMInfo(desc.Reservations[0].Instances[0])
		if vmInfo.Name == "" {
			vmInfo.Name = req.Name
		}
	} else if waitErr != nil {
		// Couldn't confirm the instance is up — surface the failure instead
		// of returning a VM that looks ready but isn't.
		return vmInfo, fmt.Errorf("instance %s created but not confirmed running: %w", instanceID, waitErr)
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
	// Always query EC2 for live state. A read-through cache here would return
	// the stale Pending snapshot recorded at create time, so health checks
	// (VMReady) would never succeed.
	resp, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{vmID},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("instance %s not found", vmID)
	}

	vm := mapEC2ToVMInfo(resp.Reservations[0].Instances[0])
	p.mu.Lock()
	p.vms[vmID] = vm
	p.mu.Unlock()
	return vm, nil
}

// ListVMs returns all VMs tagged with sandrpod
func (p *AWSProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	vms := make([]*provider.VMInfo, 0)
	var nextToken *string
	for {
		resp, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []types.Filter{
				{Name: aws.String("tag:sandrpod"), Values: []string{"true"}},
			},
			NextToken: nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to describe instances: %w", err)
		}
		for _, reservation := range resp.Reservations {
			for _, instance := range reservation.Instances {
				vms = append(vms, mapEC2ToVMInfo(instance))
			}
		}
		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}
		nextToken = resp.NextToken
	}
	return vms, nil
}

// ssmExecTimeout bounds how long ExecuteCommand waits for an SSM command to
// finish when the caller's context carries no deadline of its own.
const ssmExecTimeout = 5 * time.Minute

// ssmRegistrationTimeout bounds how long we retry SendCommand while a freshly
// launched instance is still registering with SSM (InvalidInstanceId).
const ssmRegistrationTimeout = 3 * time.Minute

// ExecuteCommand runs a shell command on a VM via SSM and waits for the result.
func (p *AWSProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	// A just-launched instance isn't an SSM-managed instance until its agent
	// registers (~1-2 min after boot). Until then SendCommand returns
	// InvalidInstanceId; retry until it's accepted or the deadline passes.
	sendCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, ssmRegistrationTimeout)
		defer cancel()
	}
	var cmdResp *ssm.SendCommandOutput
	for {
		var err error
		cmdResp, err = p.ssmClient.SendCommand(sendCtx, &ssm.SendCommandInput{
			DocumentName: aws.String("AWS-RunShellScript"),
			InstanceIds:  []string{vmID},
			Parameters:   map[string][]string{"commands": {command}},
		})
		if err == nil {
			break
		}
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceId" {
			select {
			case <-sendCtx.Done():
				return nil, fmt.Errorf("instance %s not SSM-ready before timeout: %w", vmID, err)
			case <-time.After(5 * time.Second):
				continue
			}
		}
		return nil, fmt.Errorf("failed to send command: %w", err)
	}
	commandID := aws.ToString(cmdResp.Command.CommandId)

	// Bound the wait: honor an existing context deadline, otherwise apply a
	// default. Without this, a long command (e.g. a Docker install that runs
	// for minutes) used to fall out of a fixed 30-iteration loop and be
	// reported as exit 0 — a false success.
	waitCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, ssmExecTimeout)
		defer cancel()
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("command %s on %s did not finish before timeout: %w", commandID, vmID, waitCtx.Err())
		case <-ticker.C:
		}

		inv, err := p.ssmClient.GetCommandInvocation(waitCtx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(vmID),
		})
		if err != nil {
			// The invocation may not be registered for a moment right after
			// SendCommand; keep polling until the deadline.
			continue
		}

		switch inv.Status {
		case ssmtypes.CommandInvocationStatusPending,
			ssmtypes.CommandInvocationStatusInProgress,
			ssmtypes.CommandInvocationStatusDelayed:
			continue
		}

		// Terminal state — ResponseCode is the command's real exit code.
		return &provider.CommandResult{
			Output:     strings.TrimSpace(aws.ToString(inv.StandardOutputContent)),
			Stderr:     strings.TrimSpace(aws.ToString(inv.StandardErrorContent)),
			ExitCode:   int(inv.ResponseCode),
			ExecutedAt: time.Now(),
		}, nil
	}
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
		// DescribeImages does not guarantee ordering, so pick the newest by
		// CreationDate explicitly rather than trusting Images[0].
		newest := resp.Images[0]
		for _, img := range resp.Images[1:] {
			if aws.ToString(img.CreationDate) > aws.ToString(newest.CreationDate) {
				newest = img
			}
		}
		return aws.ToString(newest.ImageId), nil
	}

	return "", fmt.Errorf("no Ubuntu 22.04 AMI found in region %s", region)
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
