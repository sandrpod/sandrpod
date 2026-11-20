// Copyright 2026 SandrPod Contributors
// What the scheduler hands a VM it booted.

package sandpod

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// fakeProvider records the remote commands the scheduler would run.
type fakeProvider struct {
	mu       sync.Mutex
	commands []string
}

func (f *fakeProvider) Name() string        { return "fakeprov" }
func (f *fakeProvider) DisplayName() string { return "Fake" }

func (f *fakeProvider) CreateVM(context.Context, *provider.CreateVMRequest) (*provider.VMInfo, error) {
	return &provider.VMInfo{ID: "vm-fake", PublicIP: "203.0.113.9"}, nil
}
func (f *fakeProvider) DeleteVM(context.Context, string) error { return nil }
func (f *fakeProvider) GetVM(context.Context, string) (*provider.VMInfo, error) {
	return &provider.VMInfo{ID: "vm-fake"}, nil
}
func (f *fakeProvider) ListVMs(context.Context) ([]*provider.VMInfo, error) { return nil, nil }

func (f *fakeProvider) ExecuteCommand(_ context.Context, _ string, command string) (*provider.CommandResult, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()
	return &provider.CommandResult{ExitCode: 0, Output: ""}, nil
}

func (f *fakeProvider) WaitUntilRunning(context.Context, string, time.Duration) error { return nil }
func (f *fakeProvider) GetHealthStatus(context.Context, string) (*provider.HealthStatus, error) {
	return &provider.HealthStatus{VMReady: true}, nil
}
func (f *fakeProvider) ListRegions(context.Context) ([]string, error) { return nil, nil }
func (f *fakeProvider) ListInstanceTypes(context.Context, string) ([]*provider.InstanceType, error) {
	return nil, nil
}
func (f *fakeProvider) GetDefaultImage(context.Context, string) (string, error) { return "img", nil }
func (f *fakeProvider) Cleanup(context.Context) error                           { return nil }

func (f *fakeProvider) startCommand(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.Contains(c, "docker run -d") {
			return c
		}
	}
	t.Fatalf("no docker run command was issued; commands: %v", f.commands)
	return ""
}

// A VM the scheduler boots must not be handed the platform token. That key is an
// administrator credential, the machine outlives the create that booted it, and
// the hop that delivers it (SSH to a host key nobody has pinned, or a cloud
// run-command API) is the one place it could be read. What must arrive instead is
// the per-VM credential for that VM and that tenant.
func TestSetupPoderOnVM_DeliversTheProvisionTokenNotThePlatformToken(t *testing.T) {
	fake := &fakeProvider{}
	if err := provider.RegisterFunc("fakeprov", "Fake", func() provider.Provider { return fake }); err != nil {
		t.Fatalf("register fake provider: %v", err)
	}

	const platformToken = "platform-admin-token"
	var mintVM, mintTenant string
	s := NewScheduler(&mockPoderRepo{}, "https://api.example", platformToken)
	s.SetTokenMinter(func(vmID, tenant string) string {
		mintVM, mintTenant = vmID, tenant
		return "pv1_minted-for-" + vmID
	})

	RecordVMTenant("vm-fake", "tenant-q")
	if err := s.setupPoderOnVM(context.Background(), "fakeprov", "region-1",
		&provider.VMInfo{ID: "vm-fake", PublicIP: "203.0.113.9"}); err != nil {
		t.Fatalf("setupPoderOnVM: %v", err)
	}

	if mintVM != "vm-fake" || mintTenant != "tenant-q" {
		t.Fatalf("minter called with (%q, %q), want the VM and the tenant that asked for it", mintVM, mintTenant)
	}

	cmd := fake.startCommand(t)
	if !strings.Contains(cmd, "SANDRPOD_TOKEN='pv1_minted-for-vm-fake'") {
		t.Errorf("the poder was not started with the per-VM credential:\n%s", cmd)
	}
	if strings.Contains(cmd, platformToken) {
		t.Errorf("the platform token reached the provisioned VM — that is the key this change removes:\n%s", cmd)
	}
	// The VM id rides along so reclamation can terminate the machine.
	if !strings.Contains(cmd, "VM_INSTANCE_ID='vm-fake'") {
		t.Errorf("VM_INSTANCE_ID missing from the poder start command:\n%s", cmd)
	}
}

// With auth disabled there is no secret to sign, so no credential is minted and
// the VM starts exactly as it did before: no SANDRPOD_TOKEN at all.
func TestSetupPoderOnVM_NoSecretMeansNoCredential(t *testing.T) {
	fake := &fakeProvider{}
	if err := provider.RegisterFunc("fakeprov-noscred", "Fake", func() provider.Provider { return fake }); err != nil {
		t.Fatalf("register fake provider: %v", err)
	}
	s := NewScheduler(&mockPoderRepo{}, "https://api.example", "")
	s.SetTokenMinter(func(string, string) string { return "" })

	if err := s.setupPoderOnVM(context.Background(), "fakeprov-noscred", "region-1",
		&provider.VMInfo{ID: "vm-anon", PublicIP: "203.0.113.10"}); err != nil {
		t.Fatalf("setupPoderOnVM: %v", err)
	}
	cmd := fake.startCommand(t)
	if strings.Contains(cmd, "SANDRPOD_TOKEN") {
		t.Errorf("a credential was delivered with no secret to sign one:\n%s", cmd)
	}
}
