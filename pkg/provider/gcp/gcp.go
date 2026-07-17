// Copyright 2026 SandrPod Contributors
// GCP Provider implementation

package gcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// labelKey is the GCP label used to mark and later find SandrPod VMs. GCP label
// values must be lowercase [a-z0-9_-], so "true" is used rather than a boolean.
const labelKey = "sandrpod"

// networkTag is applied to every VM so a firewall rule allowing SSH (tcp:22)
// can target SandrPod instances specifically.
const networkTag = "sandrpod"

// defaultImageProject/Family locate the latest Ubuntu 22.04 LTS image.
const (
	defaultImageProject = "ubuntu-os-cloud"
	defaultImageFamily  = "ubuntu-2204-lts"
)

// GCPProvider is the Google Cloud implementation of the Provider interface.
type GCPProvider struct {
	project       string
	zone          string
	network       string
	adminUsername string

	instances *compute.InstancesClient
	images    *compute.ImagesClient

	mu  sync.RWMutex
	vms map[string]*provider.VMInfo
	// sshCreds holds the per-VM ephemeral SSH key generated at CreateVM time,
	// keyed by instance name. ExecuteCommand needs it to connect. It lives only
	// in-process, which is fine: CreateVM -> bootstrap all happen in one
	// scheduler run.
	sshCreds map[string]*sshCred
}

// NewGCPProvider creates a GCP provider from the given configuration.
func NewGCPProvider(cfg *Config) (*GCPProvider, error) {
	ctx := context.Background()
	var opts []option.ClientOption
	if cfg.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsFile))
	}

	instances, err := compute.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create instances client: %w", err)
	}
	images, err := compute.NewImagesRESTClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create images client: %w", err)
	}

	return &GCPProvider{
		project:       cfg.Project,
		zone:          cfg.Zone,
		network:       cfg.Network,
		adminUsername: cfg.AdminUsername,
		instances:     instances,
		images:        images,
		vms:           make(map[string]*provider.VMInfo),
		sshCreds:      make(map[string]*sshCred),
	}, nil
}

func (p *GCPProvider) Name() string        { return "gcp" }
func (p *GCPProvider) DisplayName() string { return "Google Cloud Platform" }

// mapGCPState maps a GCP instance status to a VMState.
func mapGCPState(status string) provider.VMState {
	switch status {
	case "RUNNING":
		return provider.VMStateRunning
	case "PROVISIONING", "STAGING":
		return provider.VMStatePending
	case "STOPPING", "SUSPENDING":
		return provider.VMStateStopping
	case "TERMINATED", "STOPPED", "SUSPENDED":
		return provider.VMStateStopped
	default:
		return provider.VMStatePending
	}
}

// lastSegment returns the final path/URL segment (GCP returns machineType,
// zone, etc. as full resource URLs).
func lastSegment(url string) string {
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	return url
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func i64Ptr(i int64) *int64   { return &i }

// mapInstanceToVM converts a GCP instance to a VMInfo.
func mapInstanceToVM(inst *computepb.Instance) *provider.VMInfo {
	info := &provider.VMInfo{}
	if inst.Name != nil {
		info.ID = *inst.Name
		info.Name = *inst.Name
	}
	if inst.Zone != nil {
		info.Region = lastSegment(*inst.Zone)
	}
	if inst.MachineType != nil {
		info.InstanceType = lastSegment(*inst.MachineType)
	}
	if inst.Status != nil {
		info.State = mapGCPState(*inst.Status)
	}
	if len(inst.NetworkInterfaces) > 0 {
		ni := inst.NetworkInterfaces[0]
		if ni.NetworkIP != nil {
			info.PrivateIP = *ni.NetworkIP
		}
		if len(ni.AccessConfigs) > 0 && ni.AccessConfigs[0].NatIP != nil {
			info.PublicIP = *ni.AccessConfigs[0].NatIP
		}
	}
	if inst.CreationTimestamp != nil {
		if t, err := time.Parse(time.RFC3339, *inst.CreationTimestamp); err == nil {
			info.CreatedAt = t
		}
	}
	return info
}

// createVMIPPollTimeout bounds how long CreateVM waits for the instance to
// report a public IP after the insert operation completes.
const createVMIPPollTimeout = 90 * time.Second

// CreateVM provisions an instance with a public IP and an ephemeral SSH key
// (injected via metadata), then returns once its public IP is known. The
// scheduler uses PublicIP to bootstrap Poder over SSH.
func (p *GCPProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	zone := req.Region // GCP is zonal; the scheduler's "region" is a zone here
	if zone == "" {
		zone = p.zone
	}
	name := req.Name

	// Ephemeral SSH key: public half goes into instance metadata, private half
	// is held in-process for ExecuteCommand.
	signer, authorizedKey, err := generateSSHKey(p.adminUsername)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ssh key: %w", err)
	}

	sourceImage := req.ImageID
	if sourceImage == "" {
		sourceImage, err = p.GetDefaultImage(ctx, zone)
		if err != nil {
			return nil, fmt.Errorf("no image specified and default image lookup failed: %w", err)
		}
	}

	// Network interface: subnet when provided, else the fallback network. A
	// public IP (ONE_TO_ONE_NAT access config) is always attached — SSH needs
	// it.
	ni := &computepb.NetworkInterface{
		AccessConfigs: []*computepb.AccessConfig{{
			Name: strPtr("External NAT"),
			Type: strPtr("ONE_TO_ONE_NAT"),
		}},
	}
	if req.NetworkConfig != nil && req.NetworkConfig.SubnetID != "" {
		ni.Subnetwork = strPtr(req.NetworkConfig.SubnetID)
	} else {
		ni.Network = strPtr(p.network)
	}

	disk := &computepb.AttachedDisk{
		Boot:       boolPtr(true),
		AutoDelete: boolPtr(true),
		InitializeParams: &computepb.AttachedDiskInitializeParams{
			SourceImage: strPtr(sourceImage),
		},
	}
	if req.DiskConfig != nil && req.DiskConfig.SizeGiB > 0 {
		disk.InitializeParams.DiskSizeGb = i64Ptr(int64(req.DiskConfig.SizeGiB))
	}

	inst := &computepb.Instance{
		Name:              strPtr(name),
		MachineType:       strPtr(fmt.Sprintf("zones/%s/machineTypes/%s", zone, req.InstanceType)),
		Disks:             []*computepb.AttachedDisk{disk},
		NetworkInterfaces: []*computepb.NetworkInterface{ni},
		Tags:              &computepb.Tags{Items: []string{networkTag}},
		Labels:            buildLabels(req.Tags),
		Metadata: &computepb.Metadata{
			Items: []*computepb.Items{{
				Key:   strPtr("ssh-keys"),
				Value: strPtr(fmt.Sprintf("%s:%s", p.adminUsername, authorizedKey)),
			}},
		},
	}

	op, err := p.instances.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          p.project,
		Zone:             zone,
		InstanceResource: inst,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to insert instance: %w", err)
	}
	if err := op.Wait(ctx); err != nil {
		return nil, fmt.Errorf("instance insert did not finish: %w", err)
	}

	// Record the SSH credential before returning so ExecuteCommand can find it.
	p.mu.Lock()
	p.sshCreds[name] = &sshCred{user: p.adminUsername, signer: signer}
	p.mu.Unlock()

	// Poll for the assigned public IP.
	vmInfo := &provider.VMInfo{
		ID: name, Name: name, Region: zone,
		InstanceType: req.InstanceType, State: provider.VMStatePending,
	}
	if vm, ok := p.pollForPublicIP(ctx, zone, name); ok {
		vmInfo = vm
	}

	p.mu.Lock()
	p.vms[name] = vmInfo
	p.mu.Unlock()
	return vmInfo, nil
}

// pollForPublicIP polls Get until the instance reports a public IP or the
// bounded timeout expires. The bool reports whether at least one Get succeeded.
func (p *GCPProvider) pollForPublicIP(ctx context.Context, zone, name string) (*provider.VMInfo, bool) {
	pollCtx, cancel := context.WithTimeout(ctx, createVMIPPollTimeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastSeen *provider.VMInfo
	for {
		inst, err := p.instances.Get(pollCtx, &computepb.GetInstanceRequest{
			Project: p.project, Zone: zone, Instance: name,
		})
		if err == nil {
			vm := mapInstanceToVM(inst)
			lastSeen = vm
			if vm.PublicIP != "" {
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

// buildLabels builds the GCP label map, always tagging with the SandrPod
// marker. Caller-supplied tags are lowercased/sanitized to satisfy GCP label
// constraints; ones that can't be sanitized are dropped.
func buildLabels(tags map[string]string) map[string]string {
	labels := map[string]string{labelKey: "true"}
	for k, v := range tags {
		sk, sv := sanitizeLabel(k), sanitizeLabel(v)
		if sk != "" && sv != "" {
			labels[sk] = sv
		}
	}
	return labels
}

// sanitizeLabel lowercases a string and keeps only [a-z0-9_-], truncating to
// GCP's 63-char limit. Returns "" if nothing valid remains.
func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// DeleteVM deletes the instance (its boot disk auto-deletes) and drops the
// stored SSH credential.
func (p *GCPProvider) DeleteVM(ctx context.Context, vmID string) error {
	op, err := p.instances.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project: p.project, Zone: p.zone, Instance: vmID,
	})
	if err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("instance delete did not finish: %w", err)
	}

	p.mu.Lock()
	delete(p.vms, vmID)
	delete(p.sshCreds, vmID)
	p.mu.Unlock()
	return nil
}

// GetVM retrieves live instance info.
func (p *GCPProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	inst, err := p.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project: p.project, Zone: p.zone, Instance: vmID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}
	vm := mapInstanceToVM(inst)
	p.mu.Lock()
	p.vms[vmID] = vm
	p.mu.Unlock()
	return vm, nil
}

// ListVMs lists SandrPod-labeled instances in the configured zone.
func (p *GCPProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	it := p.instances.List(ctx, &computepb.ListInstancesRequest{
		Project: p.project,
		Zone:    p.zone,
		Filter:  strPtr(fmt.Sprintf("labels.%s=true", labelKey)),
	})
	vms := make([]*provider.VMInfo, 0)
	for {
		inst, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list instances: %w", err)
		}
		vms = append(vms, mapInstanceToVM(inst))
	}
	return vms, nil
}

// WaitUntilRunning blocks until the instance reaches RUNNING or timeout.
func (p *GCPProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
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

// GetHealthStatus reports whether the VM is running (and Docker reachable).
func (p *GCPProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	status := &provider.HealthStatus{VMReady: vm.State == provider.VMStateRunning}

	if vm.State == provider.VMStateRunning && vm.PublicIP != "" {
		checkCmd := "docker ps > /dev/null 2>&1 && echo ok || echo fail"
		if result, err := p.ExecuteCommand(ctx, vmID, checkCmd); err == nil && result.ExitCode == 0 {
			status.DockerReady = true
		}
	}
	return status, nil
}

// ListRegions returns commonly used GCP regions. A full list requires the
// regions client; this static set mirrors the AWS provider's static approach.
func (p *GCPProvider) ListRegions(ctx context.Context) ([]string, error) {
	return []string{
		"us-central1", "us-east1", "us-east4", "us-west1", "us-west2",
		"europe-west1", "europe-west2", "europe-west3", "europe-west4",
		"asia-east1", "asia-northeast1", "asia-southeast1", "australia-southeast1",
	}, nil
}

// ListInstanceTypes returns commonly used GCP machine types. Region is accepted
// for interface parity but not used to filter.
func (p *GCPProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	return []*provider.InstanceType{
		{Name: "e2-small", CPU: 2, MemoryGiB: 2},
		{Name: "e2-medium", CPU: 2, MemoryGiB: 4},
		{Name: "e2-standard-2", CPU: 2, MemoryGiB: 8},
		{Name: "e2-standard-4", CPU: 4, MemoryGiB: 16},
		{Name: "e2-standard-8", CPU: 8, MemoryGiB: 32},
		{Name: "n2-standard-2", CPU: 2, MemoryGiB: 8},
		{Name: "n2-standard-4", CPU: 4, MemoryGiB: 16},
		{Name: "c2-standard-4", CPU: 4, MemoryGiB: 16},
		{Name: "n1-standard-4", CPU: 4, MemoryGiB: 15},
		{Name: "n1-standard-4-t4", CPU: 4, MemoryGiB: 15, GPU: 1, GPUType: "NVIDIA T4"},
	}, nil
}

// GetDefaultImage returns the self-link of the latest Ubuntu 22.04 LTS image.
func (p *GCPProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	img, err := p.images.GetFromFamily(ctx, &computepb.GetFromFamilyImageRequest{
		Project: defaultImageProject,
		Family:  defaultImageFamily,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get default image: %w", err)
	}
	if img.SelfLink == nil {
		return "", fmt.Errorf("default image has no self link")
	}
	return *img.SelfLink, nil
}

// Cleanup deletes all SandrPod-managed VMs.
func (p *GCPProvider) Cleanup(ctx context.Context) error {
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
