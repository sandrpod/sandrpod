// Copyright 2026 SandrPod Contributors
// Tencent Cloud Provider implementation
//
// Tencent Cloud provides TAT (TencentCloud Automation Tools), a managed
// run-command service, so remote execution follows the same agent-based shape
// as AWS SSM / Aliyun CloudAssist rather than SSH.

package tencent

import (
	"context"
	"encoding/base64"
	"errors"
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
	zone, err := p.zoneFor(req.Region)
	if err != nil {
		return nil, err
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
	// A public IP is how the worker reaches the control plane and GHCR; a NAT
	// deployment asks for none, and this used to assign one anyway.
	runReq.InternetAccessible = internetAccessible(req.NetworkConfig)
	if err := applySpot(runReq, req); err != nil {
		return nil, err
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
		// Tencent VPC placement needs BOTH a VPC ID and a subnet ID, but the
		// scheduler's network plumbing only carries a subnet. Fail fast with a
		// clear message rather than sending an empty VpcId the API rejects.
		if nc.SubnetID != "" && nc.VpcID == "" {
			return nil, fmt.Errorf("tencent VPC placement needs both a VPC and a subnet, but only a subnet was configured — unset SANDRPOD_VM_SUBNET_ID_TENCENT to use the default VPC")
		}
		if nc.VpcID != "" {
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
	} else {
		// Returning a pending machine as if it were ready sends the scheduler
		// into a health-check wait that ends in an orphan cleanup anyway; say
		// what happened while the caller still has the create in hand.
		return vmInfo, fmt.Errorf("instance %s was created but did not reach RUNNING (or get a public IP) within %s — check the zone's capacity and the image/instance-type pairing",
			instanceID, createVMIPPollTimeout)
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
			return last, false
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
	// A delete that arrives while another termination is still in flight is
	// answered OperationDenied.InstanceOperationInProgress — a live run hit it
	// seconds after its own first delete. Retry through that window; anything
	// else (or a genuinely stuck operation) is still a failure.
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := p.cvmClient.TerminateInstancesWithContext(ctx, req); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
			if !isOperationInProgress(err) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("failed to terminate instance: %w", ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("failed to terminate instance: %w", lastErr)
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
	vms := make([]*provider.VMInfo, 0)
	var offset int64
	const limit int64 = 100
	for {
		req := cvm.NewDescribeInstancesRequest()
		req.Filters = []*cvm.Filter{{
			Name:   common.StringPtr("tag:" + tagKey),
			Values: []*string{common.StringPtr("true")},
		}}
		req.Limit = common.Int64Ptr(limit)
		req.Offset = common.Int64Ptr(offset)
		resp, err := p.cvmClient.DescribeInstancesWithContext(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("failed to list instances: %w", err)
		}
		if resp.Response == nil {
			break
		}
		for _, inst := range resp.Response.InstanceSet {
			vms = append(vms, mapInstance(inst))
		}
		total := int64(0)
		if resp.Response.TotalCount != nil {
			total = *resp.Response.TotalCount
		}
		offset += int64(len(resp.Response.InstanceSet))
		if len(resp.Response.InstanceSet) == 0 || offset >= total {
			break
		}
	}
	return vms, nil
}

const (
	tatExecTimeout         = 5 * time.Minute
	tatRegistrationTimeout = 3 * time.Minute
	// tatCommandTimeout is the command's own execution budget, sent with every
	// RunCommand. TAT defaults it to 60 seconds, and this provider left it at the
	// default: `apt-get install docker` or a cold `docker pull` on a fresh VM
	// routinely take longer, so the command was killed as TIMEOUT while the
	// catalog says it "installed Docker in under two minutes".
	tatCommandTimeout = 15 * time.Minute
)

// tatTerminalStatuses are the TAT task statuses that mean the invocation has
// finished (its ExitCode/Output are final). Non-terminal: PENDING, DELIVERING,
// DELIVER_DELAYED, RUNNING, CANCELLING.
var tatTerminalStatuses = map[string]bool{
	"SUCCESS":        true,
	"FAILED":         true, // ran, exit code != 0
	"TIMEOUT":        true, // command hit its execution timeout
	"TASK_TIMEOUT":   true, // agent/client unresponsive
	"START_FAILED":   true,
	"DELIVER_FAILED": true, // dispatch to the instance failed
	"CANCELLED":      true,
	"TERMINATED":     true,
}

// ExecuteCommand runs a shell command via TAT RunCommand and waits for the
// result. Command content and output are base64-encoded on the wire.
func (p *TencentProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	runReq := tat.NewRunCommandRequest()
	runReq.Content = common.StringPtr(base64.StdEncoding.EncodeToString([]byte(command)))
	runReq.CommandType = common.StringPtr("SHELL")
	runReq.InstanceIds = []*string{common.StringPtr(vmID)}
	// Don't persist the command definition — a fresh one is created per call.
	runReq.SaveCommand = common.BoolPtr(false)
	runReq.Timeout = common.Uint64Ptr(uint64(tatCommandTimeout.Seconds()))

	// A command that never reached the instance (DELIVER_FAILED / START_FAILED)
	// did not run, so retrying it is safe — and a live run hit DELIVER_FAILED
	// seconds after the Docker install finished, which failed a create that
	// would have succeeded on a second attempt.
	for attempt := 0; ; attempt++ {
		result, retryable, err := p.runOnce(ctx, runReq, vmID)
		if err != nil {
			return nil, err
		}
		if !retryable || attempt >= 2 {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("command on %s: %w", vmID, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// runOnce sends the command and waits for its terminal task. retryable means the
// task ended before the instance ran anything (DELIVER_FAILED, START_FAILED), so
// resending cannot double-execute.
func (p *TencentProvider) runOnce(ctx context.Context, runReq *tat.RunCommandRequest, vmID string) (*provider.CommandResult, bool, error) {
	sendCtx, cancel := context.WithTimeout(ctx, tatRegistrationTimeout)
	defer cancel()

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
				return nil, false, fmt.Errorf("instance %s TAT agent not ready before timeout: %w", vmID, err)
			case <-time.After(5 * time.Second):
				continue
			}
		}
		if err != nil {
			return nil, false, fmt.Errorf("failed to run command: %w", err)
		}
		return nil, false, fmt.Errorf("RunCommand returned no invocation id")
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
			return nil, false, fmt.Errorf("command on %s did not finish before timeout: %w", vmID, waitCtx.Err())
		case <-ticker.C:
		}

		descReq := tat.NewDescribeInvocationTasksRequest()
		descReq.Filters = []*tat.Filter{{
			Name:   common.StringPtr("invocation-id"),
			Values: []*string{common.StringPtr(invokeID)},
		}}
		descReq.HideOutput = common.BoolPtr(false)

		descResp, err := p.tatClient.DescribeInvocationTasksWithContext(waitCtx, descReq)
		if err != nil {
			// An empty set means "not visible yet" and is not an error; a real
			// API error used to be polled through until the deadline and then
			// reported as a timeout, which sends the reader to the wrong place.
			return nil, false, fmt.Errorf("describe invocation %s on %s: %w", invokeID, vmID, err)
		}
		if descResp.Response == nil || len(descResp.Response.InvocationTaskSet) == 0 {
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
		// TAT reports one combined output stream (no separate stderr). On
		// failure, mirror it into Stderr so callers that report
		// `result.Stderr` (the scheduler's bootstrap errors) get diagnostics
		// instead of an empty string. A non-SUCCESS status with exit 0 (e.g.
		// START_FAILED, DELIVER_FAILED) must also fail loudly.
		status := strVal(task.TaskStatus)
		if status != "SUCCESS" && result.ExitCode == 0 {
			result.ExitCode = 1
			if result.Output == "" {
				result.Output = "TAT task ended with status " + status
			}
		}
		if result.ExitCode != 0 && result.Stderr == "" {
			result.Stderr = result.Output
		}
		retryable := status == "DELIVER_FAILED" || status == "START_FAILED"
		return result, retryable, nil
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

// zoneFor resolves the request's region as a zone inside this client's region.
// Tencent's RunInstances takes a zone (`ap-guangzhou-3`) where every other
// provider takes a region, and a zone from another region fails deep inside the
// API with a message about the zone, not about the mismatch.
func (p *TencentProvider) zoneFor(requested string) (string, error) {
	zone := requested
	if zone == "" {
		zone = p.zone
	}
	if zone == "" {
		return "", fmt.Errorf("provider \"tencent\" needs a zone: set SANDRPOD_TENCENT_ZONE or pass the zone as the request's region (e.g. ap-guangzhou-3)")
	}
	if p.region != "" && !strings.HasPrefix(zone, p.region+"-") {
		return "", fmt.Errorf("zone %q is not in region %q: the tencent client is bound to %q, and a create names a zone inside it",
			zone, p.region, p.region)
	}
	return zone, nil
}

// internetAccessible decides the public-IP settings from the request. The
// default (no network config) keeps the historical behaviour: a public IP so
// the worker can dial out.
func internetAccessible(nc *provider.NetworkConfig) *cvm.InternetAccessible {
	if nc != nil && !nc.PublicIP {
		return nil // no public IP: the deployment routes egress itself
	}
	return &cvm.InternetAccessible{
		PublicIpAssigned:        common.BoolPtr(true),
		InternetMaxBandwidthOut: common.Int64Ptr(10),
	}
}

// applySpot turns a request for preemptible capacity into the Tencent fields:
// SPOTPAID (the API's spelling) plus a one-time market request.
//
// MaxPrice is mandatory here — RunInstances answers MissingParameter without it,
// which a live run found immediately. Tencent has no "bid the market rate"
// mode, so a deployment that wants spot has to say what it is willing to pay.
func applySpot(runReq *cvm.RunInstancesRequest, req *provider.CreateVMRequest) error {
	if !req.Spot {
		return nil
	}
	if req.SpotPriceLimit <= 0 {
		return fmt.Errorf("provider \"tencent\" needs a max price for spot capacity: set SANDRPOD_SPOT_MAX_PRICE_TENCENT (hourly, e.g. 0.05) — RunInstances rejects a spot request without MaxPrice")
	}
	runReq.InstanceChargeType = common.StringPtr("SPOTPAID")
	runReq.InstanceMarketOptions = &cvm.InstanceMarketOptionsRequest{
		MarketType: common.StringPtr("spot"),
		SpotOptions: &cvm.SpotMarketOptions{
			SpotInstanceType: common.StringPtr("one-time"),
			MaxPrice:         common.StringPtr(fmt.Sprintf("%.3f", req.SpotPriceLimit)),
		},
	}
	return nil
}

// isOperationInProgress reports the "another action on this instance is running"
// refusal, which is worth retrying for a delete: the action is frequently the
// termination this call is asking for.
func isOperationInProgress(err error) bool {
	var apiErr *sdkerrors.TencentCloudSDKError
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.Contains(apiErr.Code, "OperationInProgress")
}
