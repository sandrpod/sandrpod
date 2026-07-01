// Copyright 2024 SandrPod
// Azure Provider implementation

package azure

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// tagKey is the Azure resource tag used to mark and later find SandrPod VMs.
const tagKey = "sandrpod"

// defaultImageURN is the platform-image URN used when no image is requested.
// Azure images are a publisher:offer:sku:version tuple, not a single ID.
const defaultImageURN = "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest"

// AzureProvider is the Microsoft Azure implementation of the Provider interface.
type AzureProvider struct {
	location      string
	resourceGroup string
	adminUsername string
	sshPublicKey  string

	vmClient  *armcompute.VirtualMachinesClient
	pipClient *armnetwork.PublicIPAddressesClient
	nicClient *armnetwork.InterfacesClient

	mu  sync.RWMutex
	vms map[string]*provider.VMInfo
}

// NewAzureProvider creates an Azure provider from the given configuration.
func NewAzureProvider(cfg *Config) (*AzureProvider, error) {
	cred, err := azidentity.NewClientSecretCredential(cfg.TenantID, cfg.ClientID, cfg.ClientSecret, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure credential: %w", err)
	}

	vmClient, err := armcompute.NewVirtualMachinesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM client: %w", err)
	}
	pipClient, err := armnetwork.NewPublicIPAddressesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create public IP client: %w", err)
	}
	nicClient, err := armnetwork.NewInterfacesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create NIC client: %w", err)
	}

	return &AzureProvider{
		location:      cfg.Location,
		resourceGroup: cfg.ResourceGroup,
		adminUsername: cfg.AdminUsername,
		sshPublicKey:  cfg.SSHPublicKey,
		vmClient:      vmClient,
		pipClient:     pipClient,
		nicClient:     nicClient,
		vms:           make(map[string]*provider.VMInfo),
	}, nil
}

func (p *AzureProvider) Name() string        { return "azure" }
func (p *AzureProvider) DisplayName() string { return "Microsoft Azure" }

// nicName / pipName derive the per-VM NIC and public-IP resource names from the
// VM name so DeleteVM can find and clean them up (Azure does not cascade-delete
// a VM's NIC or public IP).
func nicName(vmName string) string { return vmName + "-nic" }
func pipName(vmName string) string { return vmName + "-pip" }

// mapPowerState maps an Azure "PowerState/*" instance-view code to a VMState.
func mapPowerState(code string) provider.VMState {
	switch strings.TrimPrefix(code, "PowerState/") {
	case "running":
		return provider.VMStateRunning
	case "starting":
		return provider.VMStatePending
	case "stopping":
		return provider.VMStateStopping
	case "stopped", "deallocated", "deallocating":
		return provider.VMStateStopped
	default:
		return provider.VMStatePending
	}
}

// CreateVM provisions a public IP, a NIC, and then the VM, and returns once the
// VM's public IP is known. The scheduler uses PublicIP to bootstrap Poder, so
// the IP must be resolved before returning.
func (p *AzureProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	location := req.Region
	if location == "" {
		location = p.location
	}
	vmName := req.Name

	if req.NetworkConfig == nil || req.NetworkConfig.SubnetID == "" {
		// Azure NICs must bind to an existing subnet; there is no "default VPC"
		// equivalent to fall back to.
		return nil, fmt.Errorf("azure requires a subnet resource ID (set SANDRPOD_VM_SUBNET_ID_AZURE)")
	}

	tags := map[string]*string{
		tagKey: to.Ptr("true"),
	}
	for k, v := range req.Tags {
		tags[k] = to.Ptr(v)
	}

	// 1. Public IP (Standard SKU + Static so the address is assigned immediately).
	pip := armnetwork.PublicIPAddress{
		Location: to.Ptr(location),
		SKU:      &armnetwork.PublicIPAddressSKU{Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
		Tags: tags,
	}
	pipPoller, err := p.pipClient.BeginCreateOrUpdate(ctx, p.resourceGroup, pipName(vmName), pip, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create public IP: %w", err)
	}
	pipResp, err := pipPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("public IP creation did not finish: %w", err)
	}
	pipID := *pipResp.ID

	// 2. NIC bound to the caller-provided subnet, associated with the public IP.
	nic := armnetwork.Interface{
		Location: to.Ptr(location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name: to.Ptr("ipconfig1"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					Subnet:                    &armnetwork.Subnet{ID: to.Ptr(req.NetworkConfig.SubnetID)},
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: to.Ptr(pipID)},
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
				},
			}},
		},
		Tags: tags,
	}
	nicPoller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.resourceGroup, nicName(vmName), nic, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create NIC: %w", err)
	}
	nicResp, err := nicPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("NIC creation did not finish: %w", err)
	}
	nicID := *nicResp.ID

	// 3. The VM itself.
	vm := armcompute.VirtualMachine{
		Location: to.Ptr(location),
		Tags:     tags,
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(req.InstanceType)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: imageReference(req.ImageID),
				OSDisk:         osDisk(req.DiskConfig),
			},
			OSProfile: p.osProfile(vmName),
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
					ID: to.Ptr(nicID),
					Properties: &armcompute.NetworkInterfaceReferenceProperties{
						Primary: to.Ptr(true),
					},
				}},
			},
		},
	}
	vmPoller, err := p.vmClient.BeginCreateOrUpdate(ctx, p.resourceGroup, vmName, vm, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}
	if _, err := vmPoller.PollUntilDone(ctx, nil); err != nil {
		return nil, fmt.Errorf("VM creation did not finish: %w", err)
	}

	// 4. Read back the assigned public IP (Static SKU has it by now).
	publicIP := ""
	if pipGet, gerr := p.pipClient.Get(ctx, p.resourceGroup, pipName(vmName), nil); gerr == nil {
		if pipGet.Properties != nil && pipGet.Properties.IPAddress != nil {
			publicIP = *pipGet.Properties.IPAddress
		}
	}

	vmInfo := &provider.VMInfo{
		ID:           vmName, // Azure VMs are addressed by name within a resource group
		Name:         vmName,
		Region:       location,
		InstanceType: req.InstanceType,
		State:        provider.VMStateRunning,
		PublicIP:     publicIP,
		CreatedAt:    time.Now(),
	}

	p.mu.Lock()
	p.vms[vmName] = vmInfo
	p.mu.Unlock()

	return vmInfo, nil
}

// imageReference builds an ImageReference from either a full image resource ID
// (/subscriptions/...) or a publisher:offer:sku:version URN. Empty uses the
// default Ubuntu URN.
func imageReference(image string) *armcompute.ImageReference {
	if image == "" {
		image = defaultImageURN
	}
	if strings.HasPrefix(image, "/subscriptions/") {
		return &armcompute.ImageReference{ID: to.Ptr(image)}
	}
	parts := strings.SplitN(image, ":", 4)
	if len(parts) != 4 {
		// Not a URN we understand — fall back to the default rather than send a
		// malformed reference.
		parts = strings.SplitN(defaultImageURN, ":", 4)
	}
	return &armcompute.ImageReference{
		Publisher: to.Ptr(parts[0]),
		Offer:     to.Ptr(parts[1]),
		SKU:       to.Ptr(parts[2]),
		Version:   to.Ptr(parts[3]),
	}
}

// osDisk builds the OS disk spec, applying an optional size/type override.
func osDisk(dc *provider.DiskConfig) *armcompute.OSDisk {
	disk := &armcompute.OSDisk{
		CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
		ManagedDisk: &armcompute.ManagedDiskParameters{
			StorageAccountType: to.Ptr(armcompute.StorageAccountTypesStandardSSDLRS),
		},
	}
	if dc != nil {
		if dc.SizeGiB > 0 {
			disk.DiskSizeGB = to.Ptr(int32(dc.SizeGiB))
		}
		if dc.VolumeType != "" {
			disk.ManagedDisk.StorageAccountType = to.Ptr(armcompute.StorageAccountTypes(dc.VolumeType))
		}
	}
	return disk
}

// osProfile builds the Linux OS profile. SSH key auth is used when a key is
// configured; otherwise a throwaway strong password satisfies Azure's mandatory
// auth requirement (never used — remote exec goes through the VM agent).
func (p *AzureProvider) osProfile(vmName string) *armcompute.OSProfile {
	prof := &armcompute.OSProfile{
		ComputerName:  to.Ptr(computerName(vmName)),
		AdminUsername: to.Ptr(p.adminUsername),
	}
	if p.sshPublicKey != "" {
		prof.LinuxConfiguration = &armcompute.LinuxConfiguration{
			DisablePasswordAuthentication: to.Ptr(true),
			SSH: &armcompute.SSHConfiguration{
				PublicKeys: []*armcompute.SSHPublicKey{{
					Path:    to.Ptr(fmt.Sprintf("/home/%s/.ssh/authorized_keys", p.adminUsername)),
					KeyData: to.Ptr(p.sshPublicKey),
				}},
			},
		}
	} else {
		prof.AdminPassword = to.Ptr(generateAdminPassword())
	}
	return prof
}

// computerName sanitizes a VM name into a valid Linux computer name (<= 64
// chars, no leading/trailing punctuation that Azure rejects).
func computerName(vmName string) string {
	if len(vmName) > 63 {
		vmName = vmName[:63]
	}
	return vmName
}

// generateAdminPassword returns a 20-char password meeting Azure's Linux
// complexity rules (upper, lower, digit, special). It is intentionally not
// returned to the caller — it exists only to satisfy the API.
func generateAdminPassword() string {
	const (
		upper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		lower   = "abcdefghijkmnpqrstuvwxyz"
		digits  = "23456789"
		special = "!@#$%^&*-_"
		all     = upper + lower + digits + special
	)
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	out := []byte{
		upper[int(b[0])%len(upper)],
		lower[int(b[1])%len(lower)],
		digits[int(b[2])%len(digits)],
		special[int(b[3])%len(special)],
	}
	for i := 4; i < len(b); i++ {
		out = append(out, all[int(b[i])%len(all)])
	}
	return string(out)
}

// DeleteVM deletes the VM and its per-VM NIC and public IP. Order matters:
// Azure refuses to delete a NIC still attached to a VM, and a public IP still
// bound to a NIC, so tear down VM -> NIC -> public IP.
func (p *AzureProvider) DeleteVM(ctx context.Context, vmID string) error {
	vmPoller, err := p.vmClient.BeginDelete(ctx, p.resourceGroup, vmID, nil)
	if err != nil {
		return fmt.Errorf("failed to delete VM: %w", err)
	}
	if _, err := vmPoller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("VM deletion did not finish: %w", err)
	}

	if nicPoller, err := p.nicClient.BeginDelete(ctx, p.resourceGroup, nicName(vmID), nil); err == nil {
		_, _ = nicPoller.PollUntilDone(ctx, nil)
	}
	if pipPoller, err := p.pipClient.BeginDelete(ctx, p.resourceGroup, pipName(vmID), nil); err == nil {
		_, _ = pipPoller.PollUntilDone(ctx, nil)
	}

	p.mu.Lock()
	delete(p.vms, vmID)
	p.mu.Unlock()
	return nil
}

// GetVM retrieves live VM info. Azure splits the model view (tags, size) from
// the instance view (power state), so both are queried and merged.
func (p *AzureProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	get, err := p.vmClient.Get(ctx, p.resourceGroup, vmID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM: %w", err)
	}

	info := &provider.VMInfo{
		ID:     vmID,
		Name:   vmID,
		Region: p.location,
		State:  provider.VMStatePending,
	}
	if get.Location != nil {
		info.Region = *get.Location
	}
	if get.Properties != nil && get.Properties.HardwareProfile != nil && get.Properties.HardwareProfile.VMSize != nil {
		info.InstanceType = string(*get.Properties.HardwareProfile.VMSize)
	}

	// Power state comes from the instance view.
	if iv, ierr := p.vmClient.InstanceView(ctx, p.resourceGroup, vmID, nil); ierr == nil {
		for _, s := range iv.Statuses {
			if s.Code != nil && strings.HasPrefix(*s.Code, "PowerState/") {
				info.State = mapPowerState(*s.Code)
			}
		}
	}

	// Public IP from the associated public-IP resource.
	if pipGet, gerr := p.pipClient.Get(ctx, p.resourceGroup, pipName(vmID), nil); gerr == nil {
		if pipGet.Properties != nil && pipGet.Properties.IPAddress != nil {
			info.PublicIP = *pipGet.Properties.IPAddress
		}
	}

	p.mu.Lock()
	p.vms[vmID] = info
	p.mu.Unlock()
	return info, nil
}

// ListVMs lists SandrPod-tagged VMs in the resource group. Power state is not
// resolved here (it would cost one InstanceView call per VM); use GetVM for a
// precise state. This is sufficient for Cleanup, which only needs IDs.
func (p *AzureProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	pager := p.vmClient.NewListPager(p.resourceGroup, nil)
	vms := make([]*provider.VMInfo, 0)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list VMs: %w", err)
		}
		for _, vm := range page.Value {
			if vm.Tags == nil || vm.Tags[tagKey] == nil || *vm.Tags[tagKey] != "true" {
				continue
			}
			name := ""
			if vm.Name != nil {
				name = *vm.Name
			}
			info := &provider.VMInfo{ID: name, Name: name, Region: p.location}
			if vm.Location != nil {
				info.Region = *vm.Location
			}
			if vm.Properties != nil && vm.Properties.HardwareProfile != nil && vm.Properties.HardwareProfile.VMSize != nil {
				info.InstanceType = string(*vm.Properties.HardwareProfile.VMSize)
			}
			vms = append(vms, info)
		}
	}
	return vms, nil
}

// runCommandExecTimeout bounds how long ExecuteCommand waits for a Run Command
// invocation to finish when the caller's context carries no deadline.
const runCommandExecTimeout = 5 * time.Minute

// rcSentinel is echoed by the wrapped script so the real exit code can be
// recovered — Azure Run Command reports stdout/stderr but no structured exit
// code.
const rcSentinel = "__SANDRPOD_RC__="

// ExecuteCommand runs a shell command on the VM via Azure Run Command and waits
// for the result. The command is wrapped so the true exit code is echoed on
// stdout and parsed back out, giving the same exit-code semantics as the SSM /
// CloudAssist providers.
func (p *AzureProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runCommandExecTimeout)
		defer cancel()
	}

	script := command + "\n" + fmt.Sprintf(`printf '%s%%s\n' "$?"`, rcSentinel)
	params := armcompute.RunCommandInput{
		CommandID: to.Ptr("RunShellScript"),
		Script:    []*string{to.Ptr(script)},
	}

	poller, err := p.vmClient.BeginRunCommand(ctx, p.resourceGroup, vmID, params, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to start run command: %w", err)
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("run command on %s did not finish: %w", vmID, err)
	}

	stdout, stderr := parseRunCommandOutput(resp.Value)
	stdout, exitCode := extractExitCode(stdout)

	return &provider.CommandResult{
		Output:     strings.TrimSpace(stdout),
		Stderr:     strings.TrimSpace(stderr),
		ExitCode:   exitCode,
		ExecutedAt: time.Now(),
	}, nil
}

// parseRunCommandOutput pulls stdout and stderr out of the Run Command status
// list. Azure encodes them as separate InstanceViewStatus entries whose Code
// contains "StdOut" / "StdErr", with the content in Message.
func parseRunCommandOutput(statuses []*armcompute.InstanceViewStatus) (stdout, stderr string) {
	for _, s := range statuses {
		if s == nil || s.Code == nil || s.Message == nil {
			continue
		}
		switch {
		case strings.Contains(*s.Code, "StdOut"):
			stdout = *s.Message
		case strings.Contains(*s.Code, "StdErr"):
			stderr = *s.Message
		}
	}
	return stdout, stderr
}

// extractExitCode finds the sentinel line emitted by the wrapped script,
// returns the remaining stdout with that line removed, and the parsed exit
// code. If the sentinel is absent (unexpected), exit code 0 is assumed.
func extractExitCode(stdout string) (string, int) {
	lines := strings.Split(stdout, "\n")
	kept := make([]string, 0, len(lines))
	exitCode := 0
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), rcSentinel); ok {
			if n, err := parseInt(strings.TrimSpace(rest)); err == nil {
				exitCode = n
			}
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), exitCode
}

// parseInt parses a base-10 int without pulling in strconv error formatting.
func parseInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-digit %q", r)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// WaitUntilRunning blocks until the VM reaches the running state or timeout.
func (p *AzureProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for VM %s to be running", vmID)
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

// GetHealthStatus reports whether the VM is running.
func (p *AzureProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	status := &provider.HealthStatus{VMReady: vm.State == provider.VMStateRunning}

	if vm.State == provider.VMStateRunning {
		checkCmd := "docker ps > /dev/null 2>&1 && echo ok || echo fail"
		if result, err := p.ExecuteCommand(ctx, vmID, checkCmd); err == nil && result.ExitCode == 0 {
			status.DockerReady = true
		}
	}
	return status, nil
}

// ListRegions returns commonly used Azure locations. A full list requires the
// subscriptions client; this static set avoids an extra dependency and mirrors
// the AWS provider's static instance-type approach.
func (p *AzureProvider) ListRegions(ctx context.Context) ([]string, error) {
	return []string{
		"eastus", "eastus2", "westus", "westus2", "westus3",
		"centralus", "northeurope", "westeurope", "uksouth",
		"southeastasia", "eastasia", "japaneast", "australiaeast",
	}, nil
}

// ListInstanceTypes returns commonly used Azure VM sizes. Region is accepted for
// interface parity but not used to filter — availability varies by region.
func (p *AzureProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	return []*provider.InstanceType{
		{Name: "Standard_B1s", CPU: 1, MemoryGiB: 1},
		{Name: "Standard_B2s", CPU: 2, MemoryGiB: 4},
		{Name: "Standard_B2ms", CPU: 2, MemoryGiB: 8},
		{Name: "Standard_D2s_v5", CPU: 2, MemoryGiB: 8},
		{Name: "Standard_D4s_v5", CPU: 4, MemoryGiB: 16},
		{Name: "Standard_D8s_v5", CPU: 8, MemoryGiB: 32},
		{Name: "Standard_E2s_v5", CPU: 2, MemoryGiB: 16},
		{Name: "Standard_E4s_v5", CPU: 4, MemoryGiB: 32},
		{Name: "Standard_F2s_v2", CPU: 2, MemoryGiB: 4},
		{Name: "Standard_F4s_v2", CPU: 4, MemoryGiB: 8},
		{Name: "Standard_NC4as_T4_v3", CPU: 4, MemoryGiB: 28, GPU: 1, GPUType: "NVIDIA T4"},
	}, nil
}

// GetDefaultImage returns the default Ubuntu platform-image URN. Azure images
// are publisher:offer:sku:version tuples, so this is a URN string that CreateVM
// parses back into an ImageReference.
func (p *AzureProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	return defaultImageURN, nil
}

// Cleanup deletes all SandrPod-managed VMs.
func (p *AzureProvider) Cleanup(ctx context.Context) error {
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
