// Copyright 2024 SandrPod
// Tencent Cloud Provider implementation
//
// Tencent Cloud provides TAT (TencentCloud Automation Tools), a managed
// run-command service, so remote execution follows the same agent-based shape
// as AWS SSM / Aliyun CloudAssist rather than SSH.

package tencent

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	sdkerrors "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
	tat "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tat/v20201028"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

const tagKey = "sandrpod"

// TencentProvider is the Tencent Cloud implementation of the Provider interface.
type TencentProvider struct {
	region string
	zone   string

	cvmClient *cvm.Client
	tatClient *tat.Client

	mu  sync.RWMutex
	vms map[string]*provider.VMInfo
}

// NewTencentProvider creates a Tencent Cloud provider from the given config.
func NewTencentProvider(cfg *Config) (*TencentProvider, error) {
	cred := common.NewCredential(cfg.SecretID, cfg.SecretKey)
	prof := profile.NewClientProfile()

	cvmClient, err := cvm.NewClient(cred, cfg.Region, prof)
	if err != nil {
		return nil, fmt.Errorf("failed to create CVM client: %w", err)
	}
	tatClient, err := tat.NewClient(cred, cfg.Region, prof)
	if err != nil {
		return nil, fmt.Errorf("failed to create TAT client: %w", err)
	}

	return &TencentProvider{
		region:    cfg.Region,
		zone:      cfg.Zone,
		cvmClient: cvmClient,
		tatClient: tatClient,
		vms:       make(map[string]*provider.VMInfo),
	}, nil
}

func (p *TencentProvider) Name() string        { return "tencent" }
func (p *TencentProvider) DisplayName() string { return "Tencent Cloud" }

func strVal(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}

// mapState maps a CVM InstanceState to a VMState.
func mapState(s string) provider.VMState {
	switch s {
	case "RUNNING":
		return provider.VMStateRunning
	case "PENDING", "STARTING", "REBOOTING":
		return provider.VMStatePending
	case "STOPPING", "TERMINATING", "SHUTDOWN":
		return provider.VMStateStopping
	case "STOPPED":
		return provider.VMStateStopped
	case "LAUNCH_FAILED":
		return provider.VMStateError
	default:
		return provider.VMStatePending
	}
}

// mapInstance converts a cvm.Instance to a VMInfo.
func mapInstance(inst *cvm.Instance) *provider.VMInfo {
	info := &provider.VMInfo{
		ID:           strVal(inst.InstanceId),
		Name:         strVal(inst.InstanceName),
		InstanceType: strVal(inst.InstanceType),
		State:        mapState(strVal(inst.InstanceState)),
	}
	if inst.Placement != nil {
		info.Region = strVal(inst.Placement.Zone)
	}
	if len(inst.PublicIpAddresses) > 0 {
		info.PublicIP = strVal(inst.PublicIpAddresses[0])
	}
	if len(inst.PrivateIpAddresses) > 0 {
		info.PrivateIP = strVal(inst.PrivateIpAddresses[0])
	}
	if t, err := time.Parse(time.RFC3339, strVal(inst.CreatedTime)); err == nil {
		info.CreatedAt = t
	}
	return info
}

const createVMIPPollTimeout = 90 * time.Second

// CreateVM creates a CVM instance with a public IP and returns once it has one.
func (p *TencentProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	zone := req.Region
	if zone == "" {
		zone = p.zone
	}
	imageID := req.ImageID
	if imageID == "" {
		resolved, err := p.GetDefaultImage(ctx, zone)
		if err != nil {
			return nil, fmt.Errorf("no image specified and default image lookup failed: %w", err)
		}
		imageID = resolved
	}

	runReq := cvm.NewRunInstancesRequest()
	runReq.Placement = &cvm.Placement{Zone: common.StringPtr(zone)}
	runReq.ImageId = common.StringPtr(imageID)
	runReq.InstanceType = common.StringPtr(req.InstanceType)
	runReq.InstanceName = common.StringPtr(req.Name)
	runReq.InstanceCount = common.Int64Ptr(1)
	// Assign a public IP so the instance can reach the API Server / pull images.
	runReq.InternetAccessible = &cvm.InternetAccessible{
		PublicIpAssigned:        common.BoolPtr(true),
		InternetMaxBandwidthOut: common.Int64Ptr(10),
	}

	tags := []*cvm.Tag{{Key: common.StringPtr(tagKey), Value: common.StringPtr("true")}}
	for k, v := range req.Tags {
		tags = append(tags, &cvm.Tag{Key: common.StringPtr(k), Value: common.StringPtr(v)})
	}
	runReq.TagSpecification = []*cvm.TagSpecification{{
		ResourceType: common.StringPtr("instance"),
		Tags:         tags,
	}}

	if req.DiskConfig != nil && req.DiskConfig.SizeGiB > 0 {
		runReq.SystemDisk = &cvm.SystemDisk{DiskSize: common.Int64Ptr(int64(req.DiskConfig.SizeGiB))}
		if req.DiskConfig.VolumeType != "" {
			runReq.SystemDisk.DiskType = common.StringPtr(req.DiskConfig.VolumeType)
		}
	}
	if nc := req.NetworkConfig; nc != nil {
		if nc.SecurityGroup != "" {
			runReq.SecurityGroupIds = []*string{common.StringPtr(nc.SecurityGroup)}
		}
		if nc.VpcID != "" || nc.SubnetID != "" {
			runReq.VirtualPrivateCloud = &cvm.VirtualPrivateCloud{
				VpcId:    common.StringPtr(nc.VpcID),
				SubnetId: common.StringPtr(nc.SubnetID),
			}
		}
	}

	resp, err := p.cvmClient.RunInstancesWithContext(ctx, runReq)
	if err != nil {
		return nil, fmt.Errorf("failed to run instance: %w", err)
	}
	if resp.Response == nil || len(resp.Response.InstanceIdSet) == 0 {
		return nil, fmt.Errorf("RunInstances returned no instance id")
	}
	instanceID := strVal(resp.Response.InstanceIdSet[0])

	vmInfo := &provider.VMInfo{
		ID: instanceID, Name: req.Name, Region: zone,
		InstanceType: req.InstanceType, State: provider.VMStatePending,
	}
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

// pollForRunningVM polls DescribeInstances until the instance is Running or has
// a public IP, or the bounded timeout expires.
func (p *TencentProvider) pollForRunningVM(ctx context.Context, instanceID string) (*provider.VMInfo, bool) {
	pollCtx, cancel := context.WithTimeout(ctx, createVMIPPollTimeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var last *provider.VMInfo
	for {
		if vm, err := p.describeOne(pollCtx, instanceID); err == nil && vm != nil {
			last = vm
			if vm.PublicIP != "" || vm.State == provider.VMStateRunning {
				return vm, true
			}
		}
		select {
		case <-pollCtx.Done():
			return last, last != nil
		case <-ticker.C:
		}
	}
}

func (p *TencentProvider) describeOne(ctx context.Context, instanceID string) (*provider.VMInfo, error) {
	req := cvm.NewDescribeInstancesRequest()
	req.InstanceIds = []*string{common.StringPtr(instanceID)}
	resp, err := p.cvmClient.DescribeInstancesWithContext(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Response == nil || len(resp.Response.InstanceSet) == 0 {
		return nil, fmt.Errorf("instance %s not found", instanceID)
	}
	return mapInstance(resp.Response.InstanceSet[0]), nil
}

// DeleteVM terminates a CVM instance.
func (p *TencentProvider) DeleteVM(ctx context.Context, vmID string) error {
	req := cvm.NewTerminateInstancesRequest()
	req.InstanceIds = []*string{common.StringPtr(vmID)}
	if _, err := p.cvmClient.TerminateInstancesWithContext(ctx, req); err != nil {
		return fmt.Errorf("failed to terminate instance: %w", err)
	}
	p.mu.Lock()
	delete(p.vms, vmID)
	p.mu.Unlock()
	return nil
}

// GetVM retrieves live instance info.
func (p *TencentProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	vm, err := p.describeOne(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}
	p.mu.Lock()
	p.vms[vmID] = vm
	p.mu.Unlock()
	return vm, nil
}

// ListVMs lists SandrPod-tagged instances.
func (p *TencentProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	req := cvm.NewDescribeInstancesRequest()
	req.Filters = []*cvm.Filter{{
		Name:   common.StringPtr("tag:" + tagKey),
		Values: []*string{common.StringPtr("true")},
	}}
	req.Limit = common.Int64Ptr(100)
	resp, err := p.cvmClient.DescribeInstancesWithContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}
	vms := make([]*provider.VMInfo, 0)
	if resp.Response != nil {
		for _, inst := range resp.Response.InstanceSet {
			vms = append(vms, mapInstance(inst))
		}
	}
	return vms, nil
}

const (
	tatExecTimeout         = 5 * time.Minute
	tatRegistrationTimeout = 3 * time.Minute
)

// tatTerminalStatuses are the TAT task statuses that mean the invocation has
// finished (its ExitCode/Output are final).
var tatTerminalStatuses = map[string]bool{
	"SUCCESS": true, "FAILED": true, "TIMEOUT": true, "START_FAILED": true,
}

// ExecuteCommand runs a shell command via TAT RunCommand and waits for the
// result. Command content and output are base64-encoded on the wire.
func (p *TencentProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	sendCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, tatRegistrationTimeout)
		defer cancel()
	}

	runReq := tat.NewRunCommandRequest()
	runReq.Content = common.StringPtr(base64.StdEncoding.EncodeToString([]byte(command)))
	runReq.CommandType = common.StringPtr("SHELL")
	runReq.InstanceIds = []*string{common.StringPtr(vmID)}
	// Don't persist the command definition — a fresh one is created per call.
	runReq.SaveCommand = common.BoolPtr(false)

	var invokeID string
	for {
		resp, err := p.tatClient.RunCommandWithContext(sendCtx, runReq)
		if err == nil && resp.Response != nil && resp.Response.InvocationId != nil {
			invokeID = *resp.Response.InvocationId
			break
		}
		if err != nil && isAgentNotReady(err) {
			select {
			case <-sendCtx.Done():
				return nil, fmt.Errorf("instance %s TAT agent not ready before timeout: %w", vmID, err)
			case <-time.After(5 * time.Second):
				continue
			}
		}
		if err != nil {
			return nil, fmt.Errorf("failed to run command: %w", err)
		}
		return nil, fmt.Errorf("RunCommand returned no invocation id")
	}

	waitCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, tatExecTimeout)
		defer cancel()
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("command on %s did not finish before timeout: %w", vmID, waitCtx.Err())
		case <-ticker.C:
		}

		descReq := tat.NewDescribeInvocationTasksRequest()
		descReq.Filters = []*tat.Filter{{
			Name:   common.StringPtr("invocation-id"),
			Values: []*string{common.StringPtr(invokeID)},
		}}
		descReq.HideOutput = common.BoolPtr(false)

		descResp, err := p.tatClient.DescribeInvocationTasksWithContext(waitCtx, descReq)
		if err != nil || descResp.Response == nil || len(descResp.Response.InvocationTaskSet) == 0 {
			continue
		}
		task := descResp.Response.InvocationTaskSet[0]
		if !tatTerminalStatuses[strVal(task.TaskStatus)] {
			continue
		}

		result := &provider.CommandResult{ExecutedAt: time.Now()}
		if task.TaskResult != nil {
			result.ExitCode = int(derefInt64(task.TaskResult.ExitCode))
			if decoded, derr := base64.StdEncoding.DecodeString(strVal(task.TaskResult.Output)); derr == nil {
				result.Output = strings.TrimSpace(string(decoded))
			} else {
				result.Output = strVal(task.TaskResult.Output)
			}
		}
		return result, nil
	}
}

func derefInt64(p *int64) int64 {
	if p != nil {
		return *p
	}
	return 0
}

// isAgentNotReady reports whether err is a TAT error indicating the target
// instance's automation agent has not come online yet.
func isAgentNotReady(err error) bool {
	var apiErr *sdkerrors.TencentCloudSDKError
	if e, ok := err.(*sdkerrors.TencentCloudSDKError); ok {
		apiErr = e
	}
	if apiErr == nil {
		return false
	}
	code := apiErr.GetCode()
	return strings.Contains(code, "Agent") || strings.Contains(code, "NotOnline") ||
		strings.Contains(code, "AgentStatus")
}

// WaitUntilRunning blocks until the instance is running or timeout.
func (p *TencentProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for instance %s", vmID)
		}
		if vm, err := p.GetVM(ctx, vmID); err == nil && vm.State == provider.VMStateRunning {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// GetHealthStatus reports whether the instance is running (and Docker reachable).
func (p *TencentProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	status := &provider.HealthStatus{VMReady: vm.State == provider.VMStateRunning}
	if vm.State == provider.VMStateRunning {
		if res, err := p.ExecuteCommand(ctx, vmID, "docker ps > /dev/null 2>&1 && echo ok || echo fail"); err == nil && res.ExitCode == 0 {
			status.DockerReady = true
		}
	}
	return status, nil
}

// ListRegions returns commonly used Tencent Cloud region IDs.
func (p *TencentProvider) ListRegions(ctx context.Context) ([]string, error) {
	return []string{
		"ap-guangzhou", "ap-shanghai", "ap-beijing", "ap-chengdu", "ap-nanjing",
		"ap-hongkong", "ap-singapore", "ap-tokyo", "ap-seoul", "na-siliconvalley",
	}, nil
}

// ListInstanceTypes returns commonly used Tencent Cloud instance types.
func (p *TencentProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	return []*provider.InstanceType{
		{Name: "S5.MEDIUM2", CPU: 2, MemoryGiB: 2},
		{Name: "S5.MEDIUM4", CPU: 2, MemoryGiB: 4},
		{Name: "S5.LARGE8", CPU: 4, MemoryGiB: 8},
		{Name: "S5.LARGE16", CPU: 4, MemoryGiB: 16},
		{Name: "S5.2XLARGE16", CPU: 8, MemoryGiB: 16},
		{Name: "S5.2XLARGE32", CPU: 8, MemoryGiB: 32},
		{Name: "SA5.MEDIUM4", CPU: 2, MemoryGiB: 4},
		{Name: "SA5.LARGE8", CPU: 4, MemoryGiB: 8},
	}, nil
}

// GetDefaultImage returns the newest public Ubuntu image ID.
func (p *TencentProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	req := cvm.NewDescribeImagesRequest()
	req.Filters = []*cvm.Filter{
		{Name: common.StringPtr("image-type"), Values: []*string{common.StringPtr("PUBLIC_IMAGE")}},
		{Name: common.StringPtr("platform"), Values: []*string{common.StringPtr("Ubuntu")}},
	}
	req.Limit = common.Uint64Ptr(100)
	resp, err := p.cvmClient.DescribeImagesWithContext(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to describe images: %w", err)
	}
	if resp.Response == nil || len(resp.Response.ImageSet) == 0 {
		return "", fmt.Errorf("no public Ubuntu image found")
	}
	// Pick the newest by CreatedTime.
	newest := resp.Response.ImageSet[0]
	for _, img := range resp.Response.ImageSet[1:] {
		if strVal(img.CreatedTime) > strVal(newest.CreatedTime) {
			newest = img
		}
	}
	return strVal(newest.ImageId), nil
}

// Cleanup deletes all SandrPod-managed instances.
func (p *TencentProvider) Cleanup(ctx context.Context) error {
	vms, err := p.ListVMs(ctx)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if err := p.DeleteVM(ctx, vm.ID); err != nil {
			fmt.Printf("failed to delete instance %s: %v\n", vm.ID, err)
		}
	}
	return nil
}
