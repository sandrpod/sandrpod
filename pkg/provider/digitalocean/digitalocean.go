// Copyright 2026 SandrPod Contributors
// DigitalOcean Provider implementation
//
// DigitalOcean has no managed run-command API, so bootstrap runs over SSH:
// CreateVM injects a per-VM ephemeral key into the droplet via cloud-init
// (root login), and ExecuteCommand connects with the shared sshexec helper.

package digitalocean

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/godo"
	"golang.org/x/crypto/ssh"

	"github.com/sandrpod/sandrpod/pkg/provider"
	"github.com/sandrpod/sandrpod/pkg/provider/sshexec"
)

// tag marks and finds SandrPod droplets.
const tag = "sandrpod"

// defaultImage is the default Ubuntu 22.04 image slug.
const defaultImage = "ubuntu-22-04-x64"

// DOProvider is the DigitalOcean implementation of the Provider interface.
type DOProvider struct {
	region string
	client *godo.Client

	mu  sync.RWMutex
	vms map[string]*provider.VMInfo
	// sshKeys holds the per-droplet ephemeral SSH signer (root login), keyed by
	// droplet ID. Held in-process, and (when keys is enabled) mirrored to disk
	// so it survives a control-plane restart.
	sshKeys map[string]ssh.Signer
	keys    *sshexec.KeyStore
}

// NewDOProvider creates a DigitalOcean provider from the given configuration.
func NewDOProvider(cfg *Config) (*DOProvider, error) {
	ks, err := sshexec.NewKeyStore(cfg.SSHKeyDir)
	if err != nil {
		return nil, err
	}
	return &DOProvider{
		region:  cfg.Region,
		client:  godo.NewFromToken(cfg.Token),
		vms:     make(map[string]*provider.VMInfo),
		sshKeys: make(map[string]ssh.Signer),
		keys:    ks,
	}, nil
}

func (p *DOProvider) Name() string        { return "digitalocean" }
func (p *DOProvider) DisplayName() string { return "DigitalOcean" }

// mapStatus maps a droplet status to a VMState.
func mapStatus(s string) provider.VMState {
	switch s {
	case "active":
		return provider.VMStateRunning
	case "new":
		return provider.VMStatePending
	case "off":
		return provider.VMStateStopped
	case "archive":
		return provider.VMStateStopped
	default:
		return provider.VMStatePending
	}
}

// mapDroplet converts a godo.Droplet to a VMInfo.
func mapDroplet(d *godo.Droplet) *provider.VMInfo {
	info := &provider.VMInfo{
		ID:    strconv.Itoa(d.ID),
		Name:  d.Name,
		State: mapStatus(d.Status),
	}
	if d.Region != nil {
		info.Region = d.Region.Slug
	}
	if d.Size != nil {
		info.InstanceType = d.Size.Slug
	}
	if ip, err := d.PublicIPv4(); err == nil {
		info.PublicIP = ip
	}
	if ip, err := d.PrivateIPv4(); err == nil {
		info.PrivateIP = ip
	}
	if t, err := time.Parse(time.RFC3339, d.Created); err == nil {
		info.CreatedAt = t
	}
	return info
}

// sanitizeTag keeps only characters DigitalOcean tags accept (letters, digits,
// colons, dashes, underscores), truncated to 255. Returns "" if nothing valid
// remains, so an unusable tag is dropped instead of failing droplet creation.
func sanitizeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == ':' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 255 {
		out = out[:255]
	}
	return out
}

const createVMIPPollTimeout = 90 * time.Second

// CreateVM creates a droplet with an ephemeral SSH key (via cloud-init) and
// returns once it has a public IP.
func (p *DOProvider) CreateVM(ctx context.Context, req *provider.CreateVMRequest) (*provider.VMInfo, error) {
	region := req.Region
	if region == "" {
		region = p.region
	}
	image := req.ImageID
	if image == "" {
		image = defaultImage
	}

	signer, authKey, priv, err := sshexec.GenerateEd25519Key("sandrpod")
	if err != nil {
		return nil, fmt.Errorf("failed to generate ssh key: %w", err)
	}

	tags := []string{tag}
	for k, v := range req.Tags {
		if t := sanitizeTag(fmt.Sprintf("%s:%s", k, v)); t != "" {
			tags = append(tags, t)
		}
	}

	createReq := &godo.DropletCreateRequest{
		Name:     req.Name,
		Region:   region,
		Size:     req.InstanceType,
		Image:    godo.DropletCreateImage{Slug: image},
		UserData: sshexec.CloudInitRootKey(authKey),
		Tags:     tags,
	}
	// VPC selection. The scheduler's network plumbing only carries SubnetID
	// (SANDRPOD_VM_SUBNET_ID_DIGITALOCEAN); DO has no subnets, so that value is
	// interpreted as the VPC UUID (documented in DIGITALOCEAN_PROVISIONING.md).
	// A directly-populated VpcID is honored too.
	if nc := req.NetworkConfig; nc != nil {
		switch {
		case nc.VpcID != "":
			createReq.VPCUUID = nc.VpcID
		case nc.SubnetID != "":
			createReq.VPCUUID = nc.SubnetID
		}
	}

	droplet, _, err := p.client.Droplets.Create(ctx, createReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create droplet: %w", err)
	}
	id := strconv.Itoa(droplet.ID)

	p.mu.Lock()
	p.sshKeys[id] = signer
	p.mu.Unlock()
	if err := p.keys.Save("digitalocean", id, priv); err != nil {
		// Non-fatal: the in-memory signer still works this process lifetime.
		fmt.Printf("digitalocean: persist ssh key for %s failed: %v\n", id, err)
	}

	vmInfo := mapDroplet(droplet)
	if vm, ok := p.pollForPublicIP(ctx, droplet.ID); ok {
		vmInfo = vm
	}

	p.mu.Lock()
	p.vms[id] = vmInfo
	p.mu.Unlock()
	return vmInfo, nil
}

// pollForPublicIP polls Get until the droplet reports a public IP or timeout.
func (p *DOProvider) pollForPublicIP(ctx context.Context, id int) (*provider.VMInfo, bool) {
	pollCtx, cancel := context.WithTimeout(ctx, createVMIPPollTimeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var last *provider.VMInfo
	for {
		if d, _, err := p.client.Droplets.Get(pollCtx, id); err == nil {
			vm := mapDroplet(d)
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

// DeleteVM deletes a droplet and drops its stored SSH key.
func (p *DOProvider) DeleteVM(ctx context.Context, vmID string) error {
	id, err := strconv.Atoi(vmID)
	if err != nil {
		return fmt.Errorf("invalid droplet id %q: %w", vmID, err)
	}
	if _, err := p.client.Droplets.Delete(ctx, id); err != nil {
		return fmt.Errorf("failed to delete droplet: %w", err)
	}
	p.mu.Lock()
	delete(p.vms, vmID)
	delete(p.sshKeys, vmID)
	p.mu.Unlock()
	p.keys.Delete("digitalocean", vmID)
	return nil
}

// GetVM retrieves live droplet info.
func (p *DOProvider) GetVM(ctx context.Context, vmID string) (*provider.VMInfo, error) {
	id, err := strconv.Atoi(vmID)
	if err != nil {
		return nil, fmt.Errorf("invalid droplet id %q: %w", vmID, err)
	}
	d, _, err := p.client.Droplets.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get droplet: %w", err)
	}
	vm := mapDroplet(d)
	p.mu.Lock()
	p.vms[vmID] = vm
	p.mu.Unlock()
	return vm, nil
}

// ListVMs lists SandrPod-tagged droplets.
func (p *DOProvider) ListVMs(ctx context.Context) ([]*provider.VMInfo, error) {
	vms := make([]*provider.VMInfo, 0)
	opt := &godo.ListOptions{PerPage: 200}
	for {
		droplets, resp, err := p.client.Droplets.ListByTag(ctx, tag, opt)
		if err != nil {
			return nil, fmt.Errorf("failed to list droplets: %w", err)
		}
		for i := range droplets {
			vms = append(vms, mapDroplet(&droplets[i]))
		}
		if resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		page, err := resp.Links.CurrentPage()
		if err != nil {
			break
		}
		opt.Page = page + 1
	}
	return vms, nil
}

// ExecuteCommand runs a command on the droplet over SSH (as root).
func (p *DOProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	p.mu.RLock()
	signer, ok := p.sshKeys[vmID]
	p.mu.RUnlock()
	if !ok {
		loaded, found, lerr := p.keys.Load("digitalocean", vmID)
		if lerr != nil {
			return nil, lerr
		}
		if !found {
			return nil, fmt.Errorf("no ssh credential for droplet %s (set SANDRPOD_SSH_KEY_DIR to persist keys across restarts)", vmID)
		}
		signer = loaded
		p.mu.Lock()
		p.sshKeys[vmID] = signer
		p.mu.Unlock()
	}
	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	if vm.PublicIP == "" {
		return nil, fmt.Errorf("droplet %s has no public IP", vmID)
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

// WaitUntilRunning blocks until the droplet is active or timeout.
func (p *DOProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for droplet %s", vmID)
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

// GetHealthStatus reports whether the droplet is running (and Docker reachable).
func (p *DOProvider) GetHealthStatus(ctx context.Context, vmID string) (*provider.HealthStatus, error) {
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

// ListRegions returns commonly used DigitalOcean region slugs.
func (p *DOProvider) ListRegions(ctx context.Context) ([]string, error) {
	return []string{
		"nyc1", "nyc3", "sfo3", "ams3", "sgp1",
		"lon1", "fra1", "tor1", "blr1", "syd1",
	}, nil
}

// ListInstanceTypes returns commonly used DigitalOcean droplet sizes.
func (p *DOProvider) ListInstanceTypes(ctx context.Context, region string) ([]*provider.InstanceType, error) {
	return []*provider.InstanceType{
		{Name: "s-1vcpu-1gb", CPU: 1, MemoryGiB: 1, DiskGiB: 25},
		{Name: "s-1vcpu-2gb", CPU: 1, MemoryGiB: 2, DiskGiB: 50},
		{Name: "s-2vcpu-2gb", CPU: 2, MemoryGiB: 2, DiskGiB: 60},
		{Name: "s-2vcpu-4gb", CPU: 2, MemoryGiB: 4, DiskGiB: 80},
		{Name: "s-4vcpu-8gb", CPU: 4, MemoryGiB: 8, DiskGiB: 160},
		{Name: "s-8vcpu-16gb", CPU: 8, MemoryGiB: 16, DiskGiB: 320},
		{Name: "c-2", CPU: 2, MemoryGiB: 4, DiskGiB: 50},
		{Name: "c-4", CPU: 4, MemoryGiB: 8, DiskGiB: 100},
	}, nil
}

// GetDefaultImage returns the default Ubuntu 22.04 image slug.
func (p *DOProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	return defaultImage, nil
}

// Cleanup deletes all SandrPod-managed droplets.
func (p *DOProvider) Cleanup(ctx context.Context) error {
	vms, err := p.ListVMs(ctx)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if err := p.DeleteVM(ctx, vm.ID); err != nil {
			fmt.Printf("failed to delete droplet %s: %v\n", vm.ID, err)
		}
	}
	return nil
}
