// Copyright 2024 SandrPod
// Scheduler - sandbox creation scheduling and orchestration logic
// Dispatches by provider type: prefers existing Poder resources and creates a new VM when none are available

package sandpod

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// DefaultAPIURL is the default API Server URL (used for local development)
var DefaultAPIURL = "http://localhost:8080"

// Scheduler dispatches sandbox creation requests to available Poder nodes
type Scheduler struct {
	poderStore PoderRepository
	apiURL     string
	token      string // API bearer token, forwarded to provisioned Poders
}

// NewScheduler creates a new Scheduler. token is the API Server bearer token
// (empty = no auth); it is forwarded to Poders started on provisioned VMs so
// they can authenticate to the tunnel endpoint.
func NewScheduler(poderStore PoderRepository, apiURL, token string) *Scheduler {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &Scheduler{
		poderStore: poderStore,
		apiURL:     apiURL,
		token:      token,
	}
}

// ScheduleSandboxCreation schedules sandbox creation.
// Flow:
// 1. Find an available Poder of the requested provider type
// 2. If one exists, create the sandbox on it directly
// 3. Otherwise, provision a new VM via the provider, wait for Poder to register, then create
func (s *Scheduler) ScheduleSandboxCreation(ctx context.Context, req *CreateSandboxRequest) (*Job, error) {
	providerType := req.ProviderType
	if providerType == "" {
		providerType = "local"
	}

	// 1. Find an available Poder
	poder, err := s.poderStore.SelectBest(req.Region, providerType)
	if err == nil && poder != nil {
		log.Printf("[Scheduler] Found available poder %s for provider %s", poder.ID, providerType)
		return s.createJobForPoder(req, poder)
	}

	// 2. No available Poder — must provision a VM first (cloud providers only)
	if providerType == "local" || providerType == "docker" {
		return nil, fmt.Errorf("no available %s poder found", providerType)
	}

	log.Printf("[Scheduler] No available poder for %s, creating new VM", providerType)

	// 3. Create VM via provider
	vm, err := s.createVMWithProvider(ctx, providerType, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// 4. Wait for VM to be ready
	log.Printf("[Scheduler] Waiting for VM %s to be ready", vm.ID)
	if err := s.waitForVMReady(ctx, providerType, vm.ID); err != nil {
		return nil, fmt.Errorf("VM not ready: %w", err)
	}

	// 5. Install Docker and start Poder on the VM
	log.Printf("[Scheduler] Setting up poder on VM %s", vm.ID)
	if err := s.setupPoderOnVM(ctx, providerType, req.Region, vm); err != nil {
		return nil, fmt.Errorf("failed to setup poder: %w", err)
	}

	// 6. Wait for Poder to register and come online
	log.Printf("[Scheduler] Waiting for poder registration for VM %s", vm.ID)
	poder = s.waitForPoderRegistration(providerType, req.Region, 5*time.Minute)
	if poder == nil {
		return nil, fmt.Errorf("poder registration timeout")
	}

	log.Printf("[Scheduler] Poder %s registered successfully", poder.ID)

	// 7. Create the job
	return s.createJobForPoder(req, poder)
}

// createVMWithProvider provisions a VM through the specified cloud provider
func (s *Scheduler) createVMWithProvider(ctx context.Context, providerType string, req *CreateSandboxRequest) (*provider.VMInfo, error) {
	p, err := provider.GetFactory().Get(providerType)
	if err != nil {
		return nil, err
	}

	createReq := &provider.CreateVMRequest{
		Name:         fmt.Sprintf("sandrpod-%s", req.Name),
		Region:       req.Region,
		InstanceType: req.InstanceType,
		ImageID:      req.ImageID,
		Tags: map[string]string{
			"CreatedBy": "sandrpod",
			"Sandbox":   req.Name,
		},
		NetworkConfig: vmNetworkConfig(providerType),
		RunnerConfig: &provider.RunnerBootstrapConfig{
			APIURL: s.apiURL,
		},
	}

	vm, err := p.CreateVM(ctx, createReq)
	if err != nil {
		return nil, err
	}

	// Contract check: when a public IP was requested, an empty PublicIP must
	// fail here rather than propagate — it would silently become PROXY_HOST=''
	// in the Poder bootstrap. (Providers return their last-seen state when the
	// IP-assignment poll times out, which can legitimately lack an IP.)
	if createReq.NetworkConfig != nil && createReq.NetworkConfig.PublicIP && vm.PublicIP == "" {
		return nil, fmt.Errorf("provider %s created VM %s but reported no public IP before timeout — the VM may still be running and need manual cleanup", providerType, vm.ID)
	}

	log.Printf("[Scheduler] VM %s created with IP %s", vm.ID, vm.PublicIP)
	return vm, nil
}

// waitForVMReady polls until the VM reports a healthy status or the deadline passes
func (s *Scheduler) waitForVMReady(ctx context.Context, providerType, vmID string) error {
	p, err := provider.GetFactory().Get(providerType)
	if err != nil {
		return err
	}

	timeout := 5 * time.Minute
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		status, err := p.GetHealthStatus(ctx, vmID)
		if err != nil {
			log.Printf("[Scheduler] Error getting health status: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}

		if status.VMReady {
			return nil
		}

		log.Printf("[Scheduler] VM %s not ready yet, waiting...", vmID)
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timeout waiting for VM %s", vmID)
}

// setupPoderOnVM installs Docker and starts the Poder container on a VM.
// region is the scheduler-facing region (not the AZ) and providerType is
// forwarded so the Poder registers under the same (region, provider_type)
// the scheduler waits on.
func (s *Scheduler) setupPoderOnVM(ctx context.Context, providerType, region string, vm *provider.VMInfo) error {
	p, err := provider.GetFactory().Get(providerType)
	if err != nil {
		return err
	}

	// 1. Install Docker. Wait for cloud-init to finish first: GCE Ubuntu images
	// rewrite the apt mirror in /etc/apt/sources.list during early boot, and
	// running apt-get (inside the Docker install) mid-rewrite hits a torn file
	// ("Type '...' is not known on line N"). The wait is BOUNDED: on some
	// clouds (observed on DigitalOcean) first-boot vendor tasks can leave
	// cloud-init in "running" indefinitely, and an unbounded
	// `cloud-init status --wait` hangs the whole bootstrap. The GCE apt race
	// clears well within 3 minutes; past that, proceed and let a real apt
	// error surface with diagnostics instead of hanging silently.
	installDocker := `timeout 180 cloud-init status --wait >/dev/null 2>&1 || true; curl -fsSL https://get.docker.com | sh`
	log.Printf("[Scheduler] Installing Docker on VM %s", vm.ID)

	result, err := p.ExecuteCommand(ctx, vm.ID, installDocker)
	if err != nil {
		return fmt.Errorf("failed to install docker: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("docker install failed: %s", result.Stderr)
	}

	// 2. Start the Poder container.
	// PROXY_HOST should be the VM's public IP so services inside the container can reach the outside.
	// Single-quote all variable values to prevent shell metacharacter injection.
	// Forward the toolbox image so the remote Poder pulls the same one we use
	// locally; otherwise it falls back to its built-in default.
	toolboxEnv := ""
	if tb := providerEnv("SANDRPOD_TOOLBOX_IMAGE", providerType); tb != "" {
		toolboxEnv = fmt.Sprintf(` -e SANDRPOD_TOOLBOX_IMAGE='%s'`, shellQuoteSingleValue(tb))
	}
	// Forward the API token so the Poder can authenticate to the tunnel
	// endpoint; without it a token-protected server rejects the handshake.
	if s.token != "" {
		toolboxEnv += fmt.Sprintf(` -e SANDRPOD_TOKEN='%s'`, shellQuoteSingleValue(s.token))
	}
	// Forward the VM instance ID so the Poder reports it on registration; the
	// server uses it to terminate the underlying cloud VM on poder reclamation.
	if vm.ID != "" {
		toolboxEnv += fmt.Sprintf(` -e VM_INSTANCE_ID='%s'`, shellQuoteSingleValue(vm.ID))
	}

	poderStartCmd := fmt.Sprintf(
		`docker run -d`+
			` --name sandrpod-poder`+
			` --restart=always`+
			` -e API_URL='%s'`+
			` -e PROXY_HOST='%s'`+
			` -e REGION='%s'`+
			` -e PROVIDER_TYPE='%s'`+
			`%s`+
			` -v /var/run/docker.sock:/var/run/docker.sock`+
			` '%s'`,
		shellQuoteSingleValue(s.apiURL),
		shellQuoteSingleValue(vm.PublicIP),
		shellQuoteSingleValue(region),
		shellQuoteSingleValue(providerType),
		toolboxEnv,
		shellQuoteSingleValue(poderImage(providerType)),
	)

	log.Printf("[Scheduler] Starting poder container on VM %s", vm.ID)

	result, err = p.ExecuteCommand(ctx, vm.ID, poderStartCmd)
	if err != nil {
		return fmt.Errorf("failed to start poder: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("poder start failed: %s", result.Stderr)
	}

	log.Printf("[Scheduler] Poder container started on VM %s", vm.ID)
	return nil
}

// waitForPoderRegistration polls poderStore until a Poder of the matching type comes online
// or the timeout expires.
func (s *Scheduler) waitForPoderRegistration(providerType, region string, timeout time.Duration) *PoderInfo {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Simple approach: return the first ONLINE Poder matching providerType and region.
		// A more precise approach would correlate by vmID, but RegisterPoderRequest does not carry a vmID field.
		poder, err := s.poderStore.SelectBest(region, providerType)
		if err == nil && poder != nil {
			return poder
		}

		log.Printf("[Scheduler] Waiting for poder registration (provider=%s, region=%s)...", providerType, region)
		time.Sleep(5 * time.Second)
	}

	return nil
}

// createJobForPoder builds a Job targeting the specified Poder
func (s *Scheduler) createJobForPoder(req *CreateSandboxRequest, poder *PoderInfo) (*Job, error) {
	job := &Job{
		ID:           GenerateJobID(),
		Type:         JobTypeCreateSandbox,
		Status:       JobStatusPending,
		SandboxName:  req.Name,
		Region:       req.Region,
		ProviderType: req.ProviderType,
		PoderID:      poder.ID,
		PoderURL:     poder.URL,
		InstanceType: req.InstanceType,
		ImageID:      req.ImageID,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	log.Printf("[Scheduler] Created job %s for poder %s (url=%s, provider=%s)",
		job.ID, poder.ID, poder.URL, req.ProviderType)

	return job, nil
}

// shellQuoteSingleValue escapes a value for safe use inside a shell single-quoted string.
// Single quotes are replaced with the '\” pattern so the string can be embedded without shell injection.
// vmNetworkConfig builds the network configuration for provisioned cloud VMs
// of the given provider. Each value is read from a provider-scoped env var
// (e.g. SANDRPOD_VM_SUBNET_ID_AWS) first and falls back to the unscoped form
// (SANDRPOD_VM_SUBNET_ID), so a single server can drive several clouds at once
// without their subnet/SG values colliding. A public IP is enabled by default
// so the VM can reach the API Server and pull images; disable with
// SANDRPOD_VM_PUBLIC_IP[_<PROVIDER>]=false for NAT/private-subnet setups.
func vmNetworkConfig(providerType string) *provider.NetworkConfig {
	return &provider.NetworkConfig{
		PublicIP:      providerEnvBool("SANDRPOD_VM_PUBLIC_IP", providerType, true),
		SubnetID:      providerEnv("SANDRPOD_VM_SUBNET_ID", providerType),
		SecurityGroup: providerEnv("SANDRPOD_VM_SECURITY_GROUP", providerType),
	}
}

// providerEnv returns a provider-scoped env var (KEY_<PROVIDER>, e.g.
// SANDRPOD_VM_SUBNET_ID_ALIYUN), falling back to the unscoped KEY. providerType
// is upper-cased for the suffix. This lets one server configure different
// networks/images per cloud while keeping the unscoped form as a shared default.
func providerEnv(key, providerType string) string {
	if providerType != "" {
		if v := strings.TrimSpace(os.Getenv(key + "_" + strings.ToUpper(providerType))); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv(key))
}

// providerEnvBool parses a provider-scoped boolean env var (KEY_<PROVIDER>),
// then the unscoped KEY, returning def when neither is set/recognized.
func providerEnvBool(key, providerType string, def bool) bool {
	if providerType != "" {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(key + "_" + strings.ToUpper(providerType)))) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return envBoolDefault(key, def)
}

// envBoolDefault parses a boolean env var, returning def when unset/unrecognized.
func envBoolDefault(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// poderImage returns the Poder container image to run on a provisioned cloud
// VM of the given provider. Override via SANDRPOD_PODER_IMAGE[_<PROVIDER>]
// (e.g. ghcr.io/<owner>/poder:<tag>, or a region-local ACR repo for Aliyun);
// it defaults to the unqualified dev image, which only resolves if it has been
// pushed to a registry the VM can reach.
func poderImage(providerType string) string {
	if img := providerEnv("SANDRPOD_PODER_IMAGE", providerType); img != "" {
		return img
	}
	return "sandrpod/poder:latest"
}

func shellQuoteSingleValue(s string) string {
	result := ""
	for _, r := range s {
		if r == '\'' {
			result += "'\\''"
		} else {
			result += string(r)
		}
	}
	return result
}
