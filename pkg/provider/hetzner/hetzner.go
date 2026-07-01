// Copyright 2024 SandrPod
// Hetzner Cloud Provider implementation
//
// Like DigitalOcean, Hetzner has no managed run-command API, so bootstrap runs
// over SSH: CreateVM injects a per-VM ephemeral key via cloud-init (root login)
// and ExecuteCommand connects with the shared sshexec helper.

package hetzner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"golang.org/x/crypto/ssh"

	"github.com/sandrpod/sandrpod/pkg/provider"
	"github.com/sandrpod/sandrpod/pkg/provider/sshexec"
)

// labelKey marks and finds SandrPod servers. Hetzner label values must be
// [a-zA-Z0-9._-], so "true" is used.
const labelKey = "sandrpod"

// defaultImage is the default Ubuntu 22.04 image name.
const defaultImage = "ubuntu-22.04"

// HetznerProvider is the Hetzner Cloud implementation of the Provider interface.
type HetznerProvider struct {
	location string
	client   *hcloud.Client

	mu  sync.RWMutex
	vms map[string]*provider.VMInfo
	// sshKeys holds the per-server ephemeral SSH signer (root), keyed by server
	// ID. Held in-process — CreateVM and bootstrap run in one process.
	sshKeys map[string]ssh.Signer
}

// NewHetznerProvider creates a Hetzner provider from the given configuration.
func NewHetznerProvider(cfg *Config) (*HetznerProvider, error) {
	return &HetznerProvider{
		location: cfg.Location,
		client:   hcloud.NewClient(hcloud.WithToken(cfg.Token)),
		vms:      make(map[string]*provider.VMInfo),
		sshKeys:  make(map[string]ssh.Signer),
	}, nil
}

func (p *HetznerProvider) Name() string        { return "hetzner" }
func (p *HetznerProvider) DisplayName() string { return "Hetzner Cloud" }

// mapStatus maps a Hetzner server status to a VMState.
func mapStatus(s string) provider.VMState {
	switch s {
	case "running":
		return provider.VMStateRunning
	case "initializing", "starting":
		return provider.VMStatePending
	case "stopping", "deleting":
		return provider.VMStateStopping
	case "off":
		return provider.VMStateStopped
	default:
		return provider.VMStatePending
	}
}

// mapServer converts an hcloud.Server to a VMInfo.
func mapServer(s *hcloud.Server) *provider.VMInfo {
	info := &provider.VMInfo{
		ID:        strconv.FormatInt(s.ID, 10),
		Name:      s.Name,
		State:     mapStatus(string(s.Status)),
		CreatedAt: s.Created,
	}
	if s.ServerType != nil {
		info.InstanceType = s.ServerType.Name
	}
	if s.Location != nil {
		info.Region = s.Location.Name
	}
	if s.PublicNet.IPv4.IP != nil {
		info.PublicIP = s.PublicNet.IPv4.IP.String()
	}
	if len(s.PrivateNet) > 0 && s.PrivateNet[0].IP != nil {
		info.PrivateIP = s.PrivateNet[0].IP.String()
	}
	return info
}

// sanitizeLabel keeps only Hetzner-legal label characters ([a-zA-Z0-9._-]),
// truncated to 63 chars.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

const createVMIPPollTimeout = 90 * time.Second

// CreateVM creates a server with an ephemeral SSH key (via cloud-init) and
// returns once it has a public IP.
func (p *HetznerProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	location := req.Region
	if location == "" {
		location = p.location
	}
	image := req.ImageID
	if image == "" {
		image = defaultImage
	}

	signer, authKey, err := sshexec.GenerateEd25519("sandrpod")
	if err != nil {
		return nil, fmt.Errorf("failed to generate ssh key: %w", err)
	}

	labels := map[string]string{labelKey: "true"}
	for k, v := range req.Tags {
		if sk, sv := sanitizeLabel(k), sanitizeLabel(v); sk != "" && sv != "" {
			labels[sk] = sv
		}
	}

	result, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       req.Name,
		ServerType: &hcloud.ServerType{Name: req.InstanceType},
		Image:      &hcloud.Image{Name: image},
		Location:   &hcloud.Location{Name: location},
		UserData:   sshexec.CloudInitRootKey(authKey),
		Labels:     labels,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}
	if result.Server == nil {
		return nil, fmt.Errorf("create returned no server")
	}
	id := strconv.FormatInt(result.Server.ID, 10)

	p.mu.Lock()
	p.sshKeys[id] = signer
	p.mu.Unlock()

	vmInfo := mapServer(result.Server)
	if vm, ok := p.pollForPublicIP(ctx, result.Server.ID); ok {
		vmInfo = vm
	}

	p.mu.Lock()
	p.vms[id] = vmInfo
	p.mu.Unlock()
	return vmInfo, nil
}

// pollForPublicIP polls GetByID until the server reports a public IP or timeout.
func (p *HetznerProvider) pollForPublicIP(ctx context.Context, id int64) (*provider.VMInfo, bool) {
	pollCtx, cancel := context.WithTimeout(ctx, createVMIPPollTimeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var last *provider.VMInfo
	for {
		if s, _, err := p.client.Server.GetByID(pollCtx, id); err == nil && s != nil {
			vm := mapServer(s)
			last = vm
			if vm.PublicIP != "" {
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

// DeleteVM deletes a server and drops its stored SSH key.
func (p *HetznerProvider) DeleteVM(ctx context.Context, vmID string) error {
	id, err := strconv.ParseInt(vmID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid server id %q: %w", vmID, err)
	}
	if _, _, err := p.client.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id}); err != nil {
		return fmt.Errorf("failed to delete server: %w", err)
	}
	p.mu.Lock()
	delete(p.vms, vmID)
	delete(p.sshKeys, vmID)
	p.mu.Unlock()
	return nil
}

// GetVM retrieves live server info.
func (p *HetznerProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	id, err := strconv.ParseInt(vmID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid server id %q: %w", vmID, err)
	}
	s, _, err := p.client.Server.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get server: %w", err)
	}
	if s == nil {
		return nil, fmt.Errorf("server %s not found", vmID)
	}
	vm := mapServer(s)
	p.mu.Lock()
	p.vms[vmID] = vm
	p.mu.Unlock()
	return vm, nil
}

// ListVMs lists SandrPod-labeled servers.
func (p *HetznerProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	servers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: labelKey + "=true"},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list servers: %w", err)
	}
	vms := make([]*provider.VMInfo, 0, len(servers))
	for _, s := range servers {
		vms = append(vms, mapServer(s))
	}
	return vms, nil
}

// ExecuteCommand runs a command on the server over SSH (as root).
func (p *HetznerProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	p.mu.RLock()
	signer, ok := p.sshKeys[vmID]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no ssh credential for server %s (created by a different process?)", vmID)
	}
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	if vm.PublicIP == "" {
		return nil, fmt.Errorf("server %s has no public IP", vmID)
	}
	res, err := sshexec.Run(ctx, vm.PublicIP, sshexec.Config{User: "root", Signer: signer}, command)
	if err != nil {
		return nil, err
	}
	return &provider.CommandResult{
		Output:     res.Stdout,
		Stderr:     res.Stderr,
		ExitCode:   res.ExitCode,
		ExecutedAt: time.Now(),
	}, nil
}

// WaitUntilRunning blocks until the server is running or timeout.
func (p *HetznerProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for server %s", vmID)
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

// GetHealthStatus reports whether the server is running (and Docker reachable).
func (p *HetznerProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	status := &provider.HealthStatus{VMReady: vm.State == provider.VMStateRunning}
	if vm.State == provider.VMStateRunning && vm.PublicIP != "" {
		if res, err := p.ExecuteCommand(ctx, vmID, "docker ps > /dev/null 2>&1 && echo ok || echo fail"); err == nil && res.ExitCode == 0 {
			status.DockerReady = true
		}
	}
	return status, nil
}

// ListRegions returns Hetzner location names.
func (p *HetznerProvider) ListRegions(ctx context.Context) ([]string, error) {
	return []string{"fsn1", "nbg1", "hel1", "ash", "hil", "sin"}, nil
}

// ListInstanceTypes returns commonly used Hetzner server types (shared vCPU).
func (p *HetznerProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	return []*provider.InstanceType{
		{Name: "cx22", CPU: 2, MemoryGiB: 4, DiskGiB: 40},
		{Name: "cx32", CPU: 4, MemoryGiB: 8, DiskGiB: 80},
		{Name: "cx42", CPU: 8, MemoryGiB: 16, DiskGiB: 160},
		{Name: "cpx11", CPU: 2, MemoryGiB: 2, DiskGiB: 40},
		{Name: "cpx21", CPU: 3, MemoryGiB: 4, DiskGiB: 80},
		{Name: "cpx31", CPU: 4, MemoryGiB: 8, DiskGiB: 160},
		{Name: "cpx41", CPU: 8, MemoryGiB: 16, DiskGiB: 240},
	}, nil
}

// GetDefaultImage returns the default Ubuntu 22.04 image name.
func (p *HetznerProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	return defaultImage, nil
}

// Cleanup deletes all SandrPod-managed servers.
func (p *HetznerProvider) Cleanup(ctx context.Context) error {
	vms, err := p.ListVMs(ctx)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if err := p.DeleteVM(ctx, vm.ID); err != nil {
			fmt.Printf("failed to delete server %s: %v\n", vm.ID, err)
		}
	}
	return nil
}
