// Copyright 2026 SandrPod Contributors
// Alibaba Cloud provider implementation

package aliyun

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	aliyunerrors "github.com/aliyun/alibaba-cloud-sdk-go/sdk/errors"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// AliyunProvider is the Alibaba Cloud implementation of the Provider interface.
type AliyunProvider struct {
	region     string
	accessKey  string
	secretKey  string
	ecsClient  *ecs.Client
	mu         sync.RWMutex
	vms        map[string]*provider.VMInfo
	imageCache map[string]string
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
		region:     cfg.Region,
		accessKey:  cfg.AccessKey,
		secretKey:  cfg.SecretKey,
		ecsClient:  client,
		vms:        make(map[string]*provider.VMInfo),
		imageCache: make(map[string]string),
	}, nil
}

// instanceIDsJSON builds the JSON-array string Aliyun's DescribeInstances
// InstanceIds field expects. Marshaling (vs fmt.Sprintf(`["%s"]`, id)) escapes
// the id, so an id containing `","` can't widen the array to query another
// instance in the same account/region.
func instanceIDsJSON(id string) string {
	b, err := json.Marshal([]string{id})
	if err != nil { // string slice never fails to marshal
		return `[]`
	}
	return string(b)
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

	// CreationTime is an RFC3339 timestamp (e.g. "2024-01-02T03:04:05Z"). Leave
	// CreatedAt zero if it's missing or malformed rather than failing the
	// whole mapping.
	createdAt := time.Time{}
	if instance.CreationTime != "" {
		if t, err := time.Parse(time.RFC3339, instance.CreationTime); err == nil {
			createdAt = t
		}
	}

	return &provider.VMInfo{
		ID:           instance.InstanceId,
		Name:         instance.InstanceName,
		Region:       instance.RegionId,
		InstanceType: instance.InstanceType,
		State:        mapInstanceState(instance.Status),
		PublicIP:     publicIP,
		PrivateIP:    privateIP,
		CreatedAt:    createdAt,
	}
}

// createVMIPPollInterval/Timeout bound how long CreateVM waits for
// DescribeInstances to report an assigned public IP after StartInstance.
const (
	createVMIPPollInterval = 5 * time.Second
	createVMIPPollTimeout  = 90 * time.Second
)

// CreateVM creates an ECS instance on Alibaba Cloud.
func (p *AliyunProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	// Resolve image ID. Fail loudly rather than silently falling back to a
	// hard-coded, region-specific image ID that may not exist or be stale.
	imageID := req.ImageID
	if imageID == "" {
		resolved, err := p.GetDefaultImage(ctx, req.Region)
		if err != nil {
			return nil, fmt.Errorf("no image specified and default image lookup failed: %w", err)
		}
		imageID = resolved
	}

	// RunInstances both creates and auto-starts the instance in a single call.
	// The older CreateInstance leaves the instance Stopped, and a separate
	// StartInstance races its initialization (IncorrectInstanceStatus), so we
	// mirror the AWS provider's RunInstances flow here.
	runReq := ecs.CreateRunInstancesRequest()
	runReq.RegionId = req.Region
	runReq.ImageId = imageID
	runReq.InstanceType = req.InstanceType
	runReq.InstanceName = req.Name
	runReq.Amount = requests.NewInteger(1)

	// Tag instances so ListVMs and Cleanup can find them.
	tags := []ecs.RunInstancesTag{{Key: "sandrpod", Value: "true"}}
	for k, v := range req.Tags {
		tags = append(tags, ecs.RunInstancesTag{Key: k, Value: v})
	}
	runReq.Tag = &tags

	// Security group
	if req.NetworkConfig != nil && req.NetworkConfig.SecurityGroup != "" {
		runReq.SecurityGroupId = req.NetworkConfig.SecurityGroup
	}

	// VSwitch (subnet). The scheduler only ever populates SubnetID (never
	// VpcID), so gating this on VpcID being set meant VSwitchId was never
	// applied and instances landed in a default/random vswitch.
	if req.NetworkConfig != nil && req.NetworkConfig.SubnetID != "" {
		runReq.VSwitchId = req.NetworkConfig.SubnetID
	}

	// Public IP
	if req.NetworkConfig != nil && req.NetworkConfig.PublicIP {
		runReq.InternetMaxBandwidthOut = requests.NewInteger(10) // 10 Mbps
	}

	// Disk configuration
	if req.DiskConfig != nil && req.DiskConfig.SizeGiB > 0 {
		runReq.SystemDiskSize = strconv.Itoa(req.DiskConfig.SizeGiB)
		runReq.SystemDiskCategory = req.DiskConfig.VolumeType
	}

	resp, err := p.ecsClient.RunInstances(runReq)
	if err != nil {
		return nil, fmt.Errorf("failed to run instance: %w", err)
	}
	if len(resp.InstanceIdSets.InstanceIdSet) == 0 {
		return nil, fmt.Errorf("RunInstances returned no instance id")
	}
	instanceID := resp.InstanceIdSets.InstanceIdSet[0]

	// Build a baseline VMInfo in case the IP-polling below never observes a
	// ready instance — callers (the scheduler) still get a usable record.
	vmInfo := &provider.VMInfo{
		ID:           instanceID,
		Name:         req.Name,
		Region:       req.Region,
		InstanceType: req.InstanceType,
		State:        provider.VMStatePending,
	}

	// Poll DescribeInstances until the instance is Running and/or has a
	// public IP assigned, or the bounded timeout/context expires. The
	// scheduler uses vm.PublicIP to bootstrap Poder over SSH, so returning
	// before the IP is assigned would leave it with nothing to connect to.
	if vm, ok := p.pollForRunningVM(ctx, instanceID); ok {
		vmInfo = vm
		if vmInfo.Name == "" {
			vmInfo.Name = req.Name
		}
	}

	p.mu.Lock()
	p.vms[instanceID] = vmInfo
	p.mu.Unlock()

	return vmInfo, nil
}

// pollForRunningVM polls DescribeInstances for up to createVMIPPollTimeout,
// returning the most recently observed VMInfo once the instance is Running
// or has a public IP, whichever comes first. The bool return reports whether
// at least one successful describe call was made (so callers can fall back
// to a baseline VMInfo on total failure).
func (p *AliyunProvider) pollForRunningVM(ctx context.Context, instanceID string) (*provider.VMInfo, bool) {
	pollCtx, cancel := context.WithTimeout(ctx, createVMIPPollTimeout)
	defer cancel()

	ticker := time.NewTicker(createVMIPPollInterval)
	defer ticker.Stop()

	var lastSeen *provider.VMInfo
	for {
		req := ecs.CreateDescribeInstancesRequest()
		req.InstanceIds = instanceIDsJSON(instanceID)
		if resp, err := p.ecsClient.DescribeInstances(req); err == nil && len(resp.Instances.Instance) > 0 {
			vm := mapEcsInstanceToVM(resp.Instances.Instance[0])
			lastSeen = vm
			if vm.PublicIP != "" || vm.State == provider.VMStateRunning {
				return vm, true
			}
		}

		select {
		case <-pollCtx.Done():
			return lastSeen, lastSeen != nil
		case <-ticker.C:
		}
	}
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

// GetVM retrieves information about a VM instance.
func (p *AliyunProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	// Always query ECS for live state. A read-through cache here would
	// return the stale Pending snapshot recorded at create time, so health
	// checks (VMReady) would never succeed.
	req := ecs.CreateDescribeInstancesRequest()
	req.InstanceIds = instanceIDsJSON(vmID)

	resp, err := p.ecsClient.DescribeInstances(req)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(resp.Instances.Instance) == 0 {
		return nil, fmt.Errorf("instance %s not found", vmID)
	}

	vm := mapEcsInstanceToVM(resp.Instances.Instance[0])
	p.mu.Lock()
	p.vms[vmID] = vm
	p.mu.Unlock()
	return vm, nil
}

// ListVMs lists all SandrPod-managed ECS instances in the configured region.
func (p *AliyunProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	vms := make([]*provider.VMInfo, 0)
	pageNumber := 1
	const pageSize = 100
	for {
		req := ecs.CreateDescribeInstancesRequest()
		req.RegionId = p.region
		req.PageNumber = requests.NewInteger(pageNumber)
		req.PageSize = requests.NewInteger(pageSize)
		// Filter to only instances created by SandrPod (via tag)
		req.Tag = &[]ecs.DescribeInstancesTag{
			{Key: "sandrpod", Value: "true"},
		}

		resp, err := p.ecsClient.DescribeInstances(req)
		if err != nil {
			return nil, fmt.Errorf("failed to describe instances: %w", err)
		}
		for _, inst := range resp.Instances.Instance {
			vms = append(vms, mapEcsInstanceToVM(inst))
		}
		// Stop when we've collected everything TotalCount reports, or the last
		// page returned fewer than a full page.
		if len(resp.Instances.Instance) < pageSize || len(vms) >= resp.TotalCount {
			break
		}
		pageNumber++
	}
	return vms, nil
}

// cloudAssistExecTimeout bounds how long ExecuteCommand waits for a
// CloudAssist invocation to reach a terminal state when the caller's
// context carries no deadline of its own. Bootstrap commands (e.g. Docker
// install) can run for minutes.
const cloudAssistExecTimeout = 5 * time.Minute

// cloudAssistRegistrationTimeout bounds how long we retry RunCommand while a
// freshly launched instance's CloudAssist agent is still starting up.
const cloudAssistRegistrationTimeout = 3 * time.Minute

// cloudAssistNotReadyErrorCodes are the Alibaba Cloud API error codes
// returned by RunCommand/InvokeCommand when the target instance's
// CloudAssist agent has not finished registering yet.
var cloudAssistNotReadyErrorCodes = map[string]bool{
	"InvalidInstance.NotActive":   true,
	"InstanceNotReady":            true,
	"InvalidParameter.InstanceId": true,
}

// invocationTerminalStatuses are the InvocationStatus values that indicate a
// CloudAssist command has finished running (successfully or not) and its
// result (ExitCode/Output) is final.
var invocationTerminalStatuses = map[string]bool{
	"Finished": true,
	"Success":  true,
	"Failed":   true,
	"Stopped":  true,
	"Timeout":  true,
}

// ExecuteCommand runs a shell command on the instance via the Alibaba Cloud
// CloudAssist service and waits for the result.
func (p *AliyunProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	// A just-launched instance's CloudAssist agent needs time to register.
	// Until then RunCommand/InvokeCommand returns an instance-not-ready
	// error code; retry until accepted or the deadline passes.
	sendCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, cloudAssistRegistrationTimeout)
		defer cancel()
	}

	runReq := ecs.CreateRunCommandRequest()
	runReq.RegionId = p.region
	runReq.Type = "RunShellScript"
	runReq.CommandContent = command
	runReq.Name = fmt.Sprintf("sandrpod-%d", time.Now().Unix())
	runReq.InstanceId = &[]string{vmID}
	// Don't keep the command definition around after it finishes — we
	// create a fresh one per ExecuteCommand call, so retaining them would
	// leak against the CloudAssist command quota.
	runReq.KeepCommand = requests.NewBoolean(false)

	var runResp *ecs.RunCommandResponse
	for {
		var err error
		runResp, err = p.ecsClient.RunCommand(runReq)
		if err == nil {
			break
		}
		if isCloudAssistNotReady(err) {
			select {
			case <-sendCtx.Done():
				return nil, fmt.Errorf("instance %s not CloudAssist-ready before timeout: %w", vmID, err)
			case <-time.After(5 * time.Second):
				continue
			}
		}
		return nil, fmt.Errorf("failed to run command: %w", err)
	}
	invokeID := runResp.InvokeId

	// Bound the wait: honor an existing context deadline, otherwise apply a
	// default long enough for slow bootstrap commands.
	waitCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, cloudAssistExecTimeout)
		defer cancel()
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("command %s on %s did not finish before timeout: %w", invokeID, vmID, waitCtx.Err())
		case <-ticker.C:
		}

		outputReq := ecs.CreateDescribeInvocationResultsRequest()
		outputReq.InstanceId = vmID
		outputReq.InvokeId = invokeID

		outputResp, err := p.ecsClient.DescribeInvocationResults(outputReq)
		if err != nil {
			// The invocation may not be registered for a moment right after
			// RunCommand; keep polling until the deadline.
			continue
		}
		if outputResp == nil || len(outputResp.Invocation.InvocationResults.InvocationResult) == 0 {
			continue
		}

		result := outputResp.Invocation.InvocationResults.InvocationResult[0]
		if !invocationTerminalStatuses[result.InvocationStatus] {
			// Still Running/Pending — Output/ExitCode aren't final yet.
			continue
		}

		stderr := result.ErrorInfo
		return &provider.CommandResult{
			Output:     result.Output,
			ExitCode:   int(result.ExitCode),
			Stderr:     stderr,
			ExecutedAt: time.Now(),
		}, nil
	}
}

// isCloudAssistNotReady reports whether err is an Alibaba Cloud API error
// indicating the target instance's CloudAssist agent isn't ready yet.
func isCloudAssistNotReady(err error) bool {
	if apiErr, ok := err.(aliyunerrors.Error); ok {
		return cloudAssistNotReadyErrorCodes[apiErr.ErrorCode()]
	}
	return false
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

// GetDefaultImage returns the default (most recently created) system image
// ID for the given region.
func (p *AliyunProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	p.mu.RLock()
	img, ok := p.imageCache[region]
	p.mu.RUnlock()
	if ok {
		return img, nil
	}

	req := ecs.CreateDescribeImagesRequest()
	req.RegionId = region
	req.ImageOwnerAlias = "system"

	resp, err := p.ecsClient.DescribeImages(req)
	if err != nil {
		return "", fmt.Errorf("failed to describe images: %w", err)
	}

	if len(resp.Images.Image) == 0 {
		return "", fmt.Errorf("no system image found in region %s", region)
	}

	// DescribeImages does not guarantee ordering, so pick the newest by
	// CreationTime explicitly rather than trusting Images[0].
	newest := resp.Images.Image[0]
	for _, candidate := range resp.Images.Image[1:] {
		if candidate.CreationTime > newest.CreationTime {
			newest = candidate
		}
	}

	p.mu.Lock()
	p.imageCache[region] = newest.ImageId
	p.mu.Unlock()

	return newest.ImageId, nil
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
