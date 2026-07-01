package digitalocean

import (
	"context"
	"testing"

	"github.com/digitalocean/godo"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

func TestMapStatus(t *testing.T) {
	cases := map[string]provider.VMState{
		"active":  provider.VMStateRunning,
		"new":     provider.VMStatePending,
		"off":     provider.VMStateStopped,
		"archive": provider.VMStateStopped,
		"weird":   provider.VMStatePending,
	}
	for in, want := range cases {
		if got := mapStatus(in); got != want {
			t.Errorf("mapStatus(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMapDroplet(t *testing.T) {
	d := &godo.Droplet{
		ID:       12345,
		Name:     "sandrpod-vm",
		Status:   "active",
		Created:  "2026-01-02T03:04:05Z",
		Region:   &godo.Region{Slug: "nyc3"},
		Size:     &godo.Size{Slug: "s-2vcpu-4gb"},
		Networks: &godo.Networks{V4: []godo.NetworkV4{{IPAddress: "1.2.3.4", Type: "public"}, {IPAddress: "10.0.0.5", Type: "private"}}},
	}
	vm := mapDroplet(d)
	if vm.ID != "12345" {
		t.Errorf("ID = %q, want 12345", vm.ID)
	}
	if vm.Name != "sandrpod-vm" || vm.Region != "nyc3" || vm.InstanceType != "s-2vcpu-4gb" {
		t.Errorf("mapping = %+v", vm)
	}
	if vm.State != provider.VMStateRunning {
		t.Errorf("State = %v", vm.State)
	}
	if vm.PublicIP != "1.2.3.4" || vm.PrivateIP != "10.0.0.5" {
		t.Errorf("IPs = %q / %q", vm.PublicIP, vm.PrivateIP)
	}
	if vm.CreatedAt.IsZero() {
		t.Error("expected parsed CreatedAt")
	}
}

func TestProviderMetadata(t *testing.T) {
	p := &DOProvider{}
	if p.Name() != "digitalocean" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DisplayName() != "DigitalOcean" {
		t.Errorf("DisplayName() = %q", p.DisplayName())
	}
}

func TestListInstanceTypes_Static(t *testing.T) {
	p := &DOProvider{}
	types, err := p.ListInstanceTypes(context.TODO(), "nyc3")
	if err != nil {
		t.Fatalf("ListInstanceTypes: %v", err)
	}
	if len(types) == 0 {
		t.Fatal("expected non-empty instance types")
	}
	for _, it := range types {
		if it.Name == "" || it.CPU <= 0 || it.MemoryGiB <= 0 {
			t.Errorf("malformed instance type: %+v", it)
		}
	}
}

func TestGetDefaultImage(t *testing.T) {
	p := &DOProvider{}
	img, _ := p.GetDefaultImage(context.TODO(), "nyc3")
	if img != "ubuntu-22-04-x64" {
		t.Errorf("default image = %q", img)
	}
}
