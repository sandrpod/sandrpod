// Copyright 2024 SandrPod
// Scheduler - sandbox creation scheduling and orchestration logic
// Dispatches by provider type: prefers existing Poder resources and creates a new VM when none are available

package sandpod

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// DefaultAPIURL is the default API Server URL (used for local development)
var DefaultAPIURL = "http://localhost:8080"

// Scheduler dispatches sandbox creation requests to available Poder nodes
type Scheduler struct {
	poderStore PoderRepository
	apiURL     string
}

// NewScheduler creates a new Scheduler
func NewScheduler(poderStore PoderRepository, apiURL string) *Scheduler {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &Scheduler{
		poderStore: poderStore,
		apiURL:     apiURL,
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
	if err := s.setupPoderOnVM(ctx, providerType, vm); err != nil {
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
	p := provider.GetFactory().MustGet(providerType)

	createReq := &provider.CreateVMRequest{
		Name:         fmt.Sprintf("sandrpod-%s", req.Name),
		Region:       req.Region,
		InstanceType: req.InstanceType,
		ImageID:      req.ImageID,
		Tags: map[string]string{
			"CreatedBy": "sandrpod",
			"Sandbox":   req.Name,
		},
		RunnerConfig: &provider.RunnerBootstrapConfig{
			APIURL: s.apiURL,
		},
	}

	vm, err := p.CreateVM(ctx, createReq)
	if err != nil {
		return nil, err
	}

	log.Printf("[Scheduler] VM %s created with IP %s", vm.ID, vm.PublicIP)
	return vm, nil
}

// waitForVMReady polls until the VM reports a healthy status or the deadline passes
func (s *Scheduler) waitForVMReady(ctx context.Context, providerType, vmID string) error {
	p := provider.GetFactory().MustGet(providerType)

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

// setupPoderOnVM installs Docker and starts the Poder container on a VM
func (s *Scheduler) setupPoderOnVM(ctx context.Context, providerType string, vm *provider.VMInfo) error {
	p := provider.GetFactory().MustGet(providerType)

	// 1. Install Docker
	installDocker := `curl -fsSL https://get.docker.com | sh`
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
	poderStartCmd := fmt.Sprintf(
		`docker run -d`+
			` --name sandrpod-poder`+
			` --restart=always`+
			` -e API_URL='%s'`+
			` -e PROXY_HOST='%s'`+
			` -e REGION='%s'`+
			` -v /var/run/docker.sock:/var/run/docker.sock`+
			` sandrpod/poder:latest`,
		shellQuoteSingleValue(s.apiURL),
		shellQuoteSingleValue(vm.PublicIP),
		shellQuoteSingleValue(vm.Region),
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
// Single quotes are replaced with the '\'' pattern so the string can be embedded without shell injection.
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
