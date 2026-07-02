// Copyright 2024 SandrPod
// Oracle Cloud Infrastructure (OCI) Provider implementation
//
// OCI has a managed run-command service (the Compute Instance Agent), so remote
// execution is agent-based like AWS/Aliyun/Azure/Tencent. Reading a launched
// instance's public IP requires walking its VNIC attachments (OCI does not put
// the public IP on the Instance object), which is done in publicIP().

package oracle

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/computeinstanceagent"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

const tagKey = "sandrpod"

// flexDefaultOcpus/GB are used when launching a Flex shape (which requires a
// shape config) and the request carries no explicit sizing.
const (
	flexDefaultOcpus float32 = 1
	flexDefaultGB    float32 = 6
)

// OracleProvider is the OCI implementation of the Provider interface.
type OracleProvider struct {
	compartmentID      string
	availabilityDomain string
	region             string

	compute core.ComputeClient
	agent   computeinstanceagent.ComputeInstanceAgentClient
	vnet    core.VirtualNetworkClient

	mu  sync.RWMutex
	vms map[string]*provider.VMInfo
}

// NewOracleProvider creates an OCI provider from the given configuration.
func NewOracleProvider(cfg *Config) (*OracleProvider, error) {
	var cp common.ConfigurationProvider
	var err error
	if cfg.ConfigFile != "" {
		cp, err = common.ConfigurationProviderFromFile(cfg.ConfigFile, "")
		if err != nil {
			return nil, fmt.Errorf("failed to load OCI config file: %w", err)
		}
	} else {
		cp = common.DefaultConfigProvider()
	}

	compute, err := core.NewComputeClientWithConfigurationProvider(cp)
	if err != nil {
		return nil, fmt.Errorf("failed to create compute client: %w", err)
	}
	agent, err := computeinstanceagent.NewComputeInstanceAgentClientWithConfigurationProvider(cp)
	if err != nil {
		return nil, fmt.Errorf("failed to create instance-agent client: %w", err)
	}
	vnet, err := core.NewVirtualNetworkClientWithConfigurationProvider(cp)
	if err != nil {
		return nil, fmt.Errorf("failed to create network client: %w", err)
	}
	region, _ := cp.Region()

	return &OracleProvider{
		compartmentID:      cfg.CompartmentID,
		availabilityDomain: cfg.AvailabilityDomain,
		region:             region,
		compute:            compute,
		agent:              agent,
		vnet:               vnet,
		vms:                make(map[string]*provider.VMInfo),
	}, nil
}

func (p *OracleProvider) Name() string        { return "oracle" }
func (p *OracleProvider) DisplayName() string { return "Oracle Cloud Infrastructure" }

// mapState maps an OCI instance lifecycle state to a VMState.
func mapState(s core.InstanceLifecycleStateEnum) provider.VMState {
	switch s {
	case core.InstanceLifecycleStateRunning:
		return provider.VMStateRunning
	case core.InstanceLifecycleStateProvisioning, core.InstanceLifecycleStateStarting:
		return provider.VMStatePending
	case core.InstanceLifecycleStateStopping, core.InstanceLifecycleStateTerminating:
		return provider.VMStateStopping
	case core.InstanceLifecycleStateStopped, core.InstanceLifecycleStateTerminated:
		return provider.VMStateStopped
	default:
		return provider.VMStatePending
	}
}

// mapInstance converts a core.Instance to a VMInfo (without the public IP,
// which is filled separately via publicIP).
func mapInstance(inst core.Instance) *provider.VMInfo {
	info := &provider.VMInfo{State: mapState(inst.LifecycleState)}
	if inst.Id != nil {
		info.ID = *inst.Id
	}
	if inst.DisplayName != nil {
		info.Name = *inst.DisplayName
	}
	if inst.Region != nil {
		info.Region = *inst.Region
	}
	if inst.Shape != nil {
		info.InstanceType = *inst.Shape
	}
	if inst.TimeCreated != nil {
		info.CreatedAt = inst.TimeCreated.Time
	}
	return info
}

const createVMIPPollTimeout = 120 * time.Second

// CreateVM launches an instance with a public IP and returns once it has one.
func (p *OracleProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	if req.NetworkConfig == nil || req.NetworkConfig.SubnetID == "" {
		return nil, fmt.Errorf("oracle requires a subnet OCID (set SANDRPOD_VM_SUBNET_ID_ORACLE)")
	}
	imageID := req.ImageID
	if imageID == "" {
		resolved, err := p.GetDefaultImage(ctx, p.region)
		if err != nil {
			return nil, fmt.Errorf("no image specified and default image lookup failed: %w", err)
		}
		imageID = resolved
	}

	details := core.LaunchInstanceDetails{
		CompartmentId:      common.String(p.compartmentID),
		AvailabilityDomain: common.String(p.availabilityDomain),
		Shape:              common.String(req.InstanceType),
		DisplayName:        common.String(req.Name),
		SourceDetails:      core.InstanceSourceViaImageDetails{ImageId: common.String(imageID)},
		CreateVnicDetails: &core.CreateVnicDetails{
			SubnetId:       common.String(req.NetworkConfig.SubnetID),
			AssignPublicIp: common.Bool(true),
		},
		FreeformTags: map[string]string{tagKey: "true"},
	}
	maps.Copy(details.FreeformTags, req.Tags)
	// Flex shapes require a shape config.
	if strings.Contains(strings.ToLower(req.InstanceType), "flex") {
		ocpus, gb := flexDefaultOcpus, flexDefaultGB
		details.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{Ocpus: &ocpus, MemoryInGBs: &gb}
	}

	resp, err := p.compute.LaunchInstance(ctx, core.LaunchInstanceRequest{LaunchInstanceDetails: details})
	if err != nil {
		return nil, fmt.Errorf("failed to launch instance: %w", err)
	}
	if resp.Id == nil {
		return nil, fmt.Errorf("launch returned no instance id")
	}
	instanceID := *resp.Id

	vmInfo := mapInstance(resp.Instance)
	if vm, ok := p.pollForRunningVM(ctx, instanceID); ok {
		vmInfo = vm
	}

	p.mu.Lock()
	p.vms[instanceID] = vmInfo
	p.mu.Unlock()
	return vmInfo, nil
}

// pollForRunningVM polls until the instance is Running, then resolves its public
// IP (only available once a VNIC is attached).
func (p *OracleProvider) pollForRunningVM(ctx context.Context, instanceID string) (*provider.VMInfo, bool) {
	pollCtx, cancel := context.WithTimeout(ctx, createVMIPPollTimeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var last *provider.VMInfo
	for {
		if vm, err := p.getInstance(pollCtx, instanceID); err == nil {
			last = vm
			if vm.State == provider.VMStateRunning {
				if ip, err := p.publicIP(pollCtx, instanceID); err == nil && ip != "" {
					vm.PublicIP = ip
				}
				if vm.PublicIP != "" {
					return vm, true
				}
			}
		}
		select {
		case <-pollCtx.Done():
			return last, last != nil
		case <-ticker.C:
		}
	}
}

func (p *OracleProvider) getInstance(ctx context.Context, instanceID string) (*provider.VMInfo, error) {
	resp, err := p.compute.GetInstance(ctx, core.GetInstanceRequest{InstanceId: common.String(instanceID)})
	if err != nil {
		return nil, err
	}
	return mapInstance(resp.Instance), nil
}

// publicIP walks the instance's VNIC attachments to find its public IP.
func (p *OracleProvider) publicIP(ctx context.Context, instanceID string) (string, error) {
	att, err := p.compute.ListVnicAttachments(ctx, core.ListVnicAttachmentsRequest{
		CompartmentId: common.String(p.compartmentID),
		InstanceId:    common.String(instanceID),
	})
	if err != nil {
		return "", err
	}
	for _, a := range att.Items {
		if a.VnicId == nil {
			continue
		}
		vnic, err := p.vnet.GetVnic(ctx, core.GetVnicRequest{VnicId: a.VnicId})
		if err != nil {
			continue
		}
		if vnic.PublicIp != nil && *vnic.PublicIp != "" {
			return *vnic.PublicIp, nil
		}
	}
	return "", nil
}

// DeleteVM terminates an instance (and its boot volume).
func (p *OracleProvider) DeleteVM(ctx context.Context, vmID string) error {
	_, err := p.compute.TerminateInstance(ctx, core.TerminateInstanceRequest{
		InstanceId:         common.String(vmID),
		PreserveBootVolume: common.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instance: %w", err)
	}
	p.mu.Lock()
	delete(p.vms, vmID)
	p.mu.Unlock()
	return nil
}

// GetVM retrieves live instance info including its public IP.
func (p *OracleProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	vm, err := p.getInstance(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}
	if ip, err := p.publicIP(ctx, vmID); err == nil && ip != "" {
		vm.PublicIP = ip
	}
	p.mu.Lock()
	p.vms[vmID] = vm
	p.mu.Unlock()
	return vm, nil
}

// ListVMs lists SandrPod-tagged instances in the compartment, following
// pagination so instances beyond the first page aren't silently invisible
// (Cleanup relies on this listing being complete).
func (p *OracleProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	vms := make([]*provider.VMInfo, 0)
	req := core.ListInstancesRequest{CompartmentId: common.String(p.compartmentID)}
	for {
		resp, err := p.compute.ListInstances(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("failed to list instances: %w", err)
		}
		for _, inst := range resp.Items {
			if inst.FreeformTags[tagKey] != "true" {
				continue
			}
			if inst.LifecycleState == core.InstanceLifecycleStateTerminated {
				continue
			}
			vms = append(vms, mapInstance(inst))
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			break
		}
		req.Page = resp.OpcNextPage
	}
	return vms, nil
}

const (
	agentExecTimeout         = 5 * time.Minute
	agentRegistrationTimeout = 3 * time.Minute
	agentExecTimeoutSeconds  = 300
)

// agentTerminalStates are the execution lifecycle states that mean the command
// has finished.
var agentTerminalStates = map[computeinstanceagent.InstanceAgentCommandExecutionLifecycleStateEnum]bool{
	computeinstanceagent.InstanceAgentCommandExecutionLifecycleStateSucceeded: true,
	computeinstanceagent.InstanceAgentCommandExecutionLifecycleStateFailed:    true,
	computeinstanceagent.InstanceAgentCommandExecutionLifecycleStateTimedOut:  true,
}

// ExecuteCommand runs a shell command via the Compute Instance Agent and waits
// for the result.
func (p *OracleProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	sendCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, agentRegistrationTimeout)
		defer cancel()
	}

	createReq := computeinstanceagent.CreateInstanceAgentCommandRequest{
		CreateInstanceAgentCommandDetails: computeinstanceagent.CreateInstanceAgentCommandDetails{
			CompartmentId:             common.String(p.compartmentID),
			ExecutionTimeOutInSeconds: common.Int(agentExecTimeoutSeconds),
			Target:                    &computeinstanceagent.InstanceAgentCommandTarget{InstanceId: common.String(vmID)},
			Content: &computeinstanceagent.InstanceAgentCommandContent{
				Source: computeinstanceagent.InstanceAgentCommandSourceViaTextDetails{Text: common.String(command)},
				Output: computeinstanceagent.InstanceAgentCommandOutputViaTextDetails{},
			},
		},
	}

	// A just-launched instance's agent needs time to register; retry until the
	// command is accepted or the registration window closes. Only transient
	// conditions are retried — permanent errors (bad compartment, not
	// authorized, malformed request) surface immediately with their real cause
	// instead of a misleading "agent not ready" after 3 minutes.
	var commandID string
	for {
		resp, err := p.agent.CreateInstanceAgentCommand(sendCtx, createReq)
		if err == nil && resp.Id != nil {
			commandID = *resp.Id
			break
		}
		if err != nil {
			if !isRetryableAgentErr(err) {
				return nil, fmt.Errorf("failed to create agent command: %w", err)
			}
			select {
			case <-sendCtx.Done():
				return nil, fmt.Errorf("instance %s agent not ready before timeout: %w", vmID, err)
			case <-time.After(5 * time.Second):
				continue
			}
		}
		return nil, fmt.Errorf("CreateInstanceAgentCommand returned no id")
	}

	waitCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, agentExecTimeout)
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

		exec, err := p.agent.GetInstanceAgentCommandExecution(waitCtx, computeinstanceagent.GetInstanceAgentCommandExecutionRequest{
			InstanceAgentCommandId: common.String(commandID),
			InstanceId:             common.String(vmID),
		})
		if err != nil {
			continue
		}
		if !agentTerminalStates[exec.LifecycleState] {
			continue
		}

		result := &provider.CommandResult{ExecutedAt: time.Now()}
		txt, ok := exec.Content.(computeinstanceagent.InstanceAgentCommandExecutionOutputViaTextDetails)
		if !ok {
			// We always request text output, so any other content shape is
			// unexpected. Failing loudly beats returning the zero value —
			// ExitCode 0 with empty output would read as a silent success.
			return nil, fmt.Errorf("unexpected agent command output content type %T (state %s)", exec.Content, exec.LifecycleState)
		}
		if txt.ExitCode != nil {
			result.ExitCode = *txt.ExitCode
		}
		if txt.Text != nil {
			result.Output = strings.TrimSpace(*txt.Text)
		}
		if txt.Message != nil {
			result.Stderr = strings.TrimSpace(*txt.Message)
		}
		// A non-SUCCEEDED terminal state must not read as success even if the
		// service reported no exit code (e.g. the command never started).
		if exec.LifecycleState != computeinstanceagent.InstanceAgentCommandExecutionLifecycleStateSucceeded && result.ExitCode == 0 {
			result.ExitCode = 1
			if result.Stderr == "" {
				result.Stderr = fmt.Sprintf("agent command ended in state %s", exec.LifecycleState)
			}
		}
		return result, nil
	}
}

// isRetryableAgentErr reports whether a CreateInstanceAgentCommand error is
// worth retrying while a fresh instance's agent registers. 404 (agent/instance
// not known to the agent service yet), 409 (conflict), 429 (throttled), and
// 5xx are transient; 4xx like 400/401/403 are permanent and surface
// immediately. Non-service errors (network) are treated as transient.
func isRetryableAgentErr(err error) bool {
	if svcErr, ok := common.IsServiceError(err); ok {
		switch code := svcErr.GetHTTPStatusCode(); {
		case code == 404 || code == 409 || code == 429:
			return true
		case code >= 500:
			return true
		default:
			return false
		}
	}
	return true
}

// WaitUntilRunning blocks until the instance is running or timeout.
func (p *OracleProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for instance %s", vmID)
		}
		if vm, err := p.getInstance(ctx, vmID); err == nil && vm.State == provider.VMStateRunning {
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
func (p *OracleProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
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

// ListRegions returns commonly used OCI region identifiers.
func (p *OracleProvider) ListRegions(ctx context.Context) ([]string, error) {
	return []string{
		"us-ashburn-1", "us-phoenix-1", "us-sanjose-1",
		"eu-frankfurt-1", "eu-amsterdam-1", "uk-london-1",
		"ap-tokyo-1", "ap-singapore-1", "ap-mumbai-1", "ap-sydney-1",
	}, nil
}

// ListInstanceTypes returns commonly used OCI shapes.
func (p *OracleProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	return []*provider.InstanceType{
		{Name: "VM.Standard.E2.1.Micro", CPU: 1, MemoryGiB: 1},
		{Name: "VM.Standard.A1.Flex", CPU: 1, MemoryGiB: 6},
		{Name: "VM.Standard.E4.Flex", CPU: 1, MemoryGiB: 8},
		{Name: "VM.Standard.E5.Flex", CPU: 2, MemoryGiB: 16},
		{Name: "VM.Standard3.Flex", CPU: 2, MemoryGiB: 16},
	}, nil
}

// GetDefaultImage returns the newest Canonical Ubuntu 22.04 image OCID in the
// compartment.
func (p *OracleProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	resp, err := p.compute.ListImages(ctx, core.ListImagesRequest{
		CompartmentId:          common.String(p.compartmentID),
		OperatingSystem:        common.String("Canonical Ubuntu"),
		OperatingSystemVersion: common.String("22.04"),
		SortBy:                 core.ListImagesSortByTimecreated,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list images: %w", err)
	}
	if len(resp.Items) == 0 || resp.Items[0].Id == nil {
		return "", fmt.Errorf("no Canonical Ubuntu 22.04 image found")
	}
	// SortBy=timeCreated defaults to descending (newest first).
	return *resp.Items[0].Id, nil
}

// Cleanup deletes all SandrPod-managed instances.
func (p *OracleProvider) Cleanup(ctx context.Context) error {
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
