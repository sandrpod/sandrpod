package sandpod

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// leakProvider creates VMs happily and fails to bootstrap them — the shape of
// an AWS account with no instance profile, where RunInstances succeeds and SSM
// can never reach the instance.
type leakProvider struct {
	name      string
	created   atomic.Int32
	deleted   atomic.Int32
	deleteErr error
}

func (p *leakProvider) Name() string        { return p.name }
func (p *leakProvider) DisplayName() string { return p.name }
func (p *leakProvider) CreateVM(context.Context, *provider.CreateVMRequest) (*provider.VMInfo, error) {
	p.created.Add(1)
	return &provider.VMInfo{ID: "i-orphan", State: provider.VMStateRunning, PublicIP: "203.0.113.9"}, nil
}
func (p *leakProvider) DeleteVM(_ context.Context, _ string) error {
	p.deleted.Add(1)
	return p.deleteErr
}
func (p *leakProvider) GetVM(context.Context, string) (*provider.VMInfo, error) {
	return &provider.VMInfo{ID: "i-orphan", State: provider.VMStateRunning}, nil
}
func (p *leakProvider) ListVMs(context.Context) ([]*provider.VMInfo, error) { return nil, nil }
func (p *leakProvider) ExecuteCommand(context.Context, string, string) (*provider.CommandResult, error) {
	// What SSM reports when the instance never became SSM-managed.
	return nil, errors.New("instance i-orphan not SSM-ready before timeout")
}
func (p *leakProvider) WaitUntilRunning(context.Context, string, time.Duration) error { return nil }
func (p *leakProvider) GetHealthStatus(context.Context, string) (*provider.HealthStatus, error) {
	return &provider.HealthStatus{VMReady: true}, nil
}
func (p *leakProvider) ListRegions(context.Context) ([]string, error) { return nil, nil }
func (p *leakProvider) ListInstanceTypes(context.Context, string) ([]*provider.InstanceType, error) {
	return nil, nil
}
func (p *leakProvider) GetDefaultImage(context.Context, string) (string, error) { return "ami-x", nil }
func (p *leakProvider) Cleanup(context.Context) error                           { return nil }

func withProvider(t *testing.T, p provider.Provider) {
	t.Helper()
	if err := provider.GetFactory().Register(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.GetFactory().Unregister(p.Name()) })
}

// A VM that fails to bootstrap never registers a poder, so no reaper can ever
// see it — they all iterate the poder store, and nothing reconciles against the
// provider's VM list. Before this, every failed provision left an instance
// running and billing until someone found it in the cloud console.
func TestScheduleCloud_TerminatesVMWhenBootstrapFails(t *testing.T) {
	p := &leakProvider{name: "leaky"}
	withProvider(t, p)

	repo := &mockPoderRepo{
		selectBestFn: func(string, string) (*PoderInfo, error) {
			return nil, errors.New("no poder available")
		},
	}
	s := NewScheduler(repo, "http://api", "tok")
	_, err := s.ScheduleSandboxCreation(context.Background(),
		&CreateSandboxRequest{Name: "demo", ProviderType: "leaky", Region: "us-east-1"})

	if err == nil {
		t.Fatal("expected the bootstrap failure to surface")
	}
	if p.created.Load() != 1 {
		t.Fatalf("created %d VMs, want 1", p.created.Load())
	}
	if got := p.deleted.Load(); got != 1 {
		t.Errorf("terminated %d VMs, want 1 — the instance is still billing", got)
	}
	if !strings.Contains(err.Error(), "terminated") {
		t.Errorf("error %q does not tell the caller the VM was given back", err)
	}
}

// When the cleanup itself fails there is nothing more to do but say so loudly:
// the caller has to remove it by hand, and needs the id to do it.
func TestScheduleCloud_SaysSoWhenTerminateFails(t *testing.T) {
	p := &leakProvider{name: "leaky-undeletable", deleteErr: errors.New("AuthFailure")}
	withProvider(t, p)

	repo := &mockPoderRepo{
		selectBestFn: func(string, string) (*PoderInfo, error) {
			return nil, errors.New("no poder available")
		},
	}
	s := NewScheduler(repo, "http://api", "tok")
	_, err := s.ScheduleSandboxCreation(context.Background(),
		&CreateSandboxRequest{Name: "demo", ProviderType: "leaky-undeletable", Region: "us-east-1"})

	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"i-orphan", "left running", "by hand"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
