package hetzner

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

func TestMapStatus(t *testing.T) {
	cases := map[string]provider.VMState{
		"running":      provider.VMStateRunning,
		"initializing": provider.VMStatePending,
		"starting":     provider.VMStatePending,
		"stopping":     provider.VMStateStopping,
		"deleting":     provider.VMStateStopping,
		"off":          provider.VMStateStopped,
		"migrating":    provider.VMStatePending, // no dedicated state; treated as transitional
		"rebuilding":   provider.VMStatePending,
		"unknown":      provider.VMStatePending,
		"weird":        provider.VMStatePending,
	}
	for in, want := range cases {
		if got := mapStatus(in); got != want {
			t.Errorf("mapStatus(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMapServer(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := &hcloud.Server{
		ID:         987654,
		Name:       "sandrpod-vm",
		Status:     hcloud.ServerStatusRunning,
		Created:    created,
		ServerType: &hcloud.ServerType{Name: "cx22"},
		Location:   &hcloud.Location{Name: "fsn1"},
		PublicNet:  hcloud.ServerPublicNet{IPv4: hcloud.ServerPublicNetIPv4{IP: net.ParseIP("1.2.3.4")}},
	}
	vm := mapServer(s)
	if vm.ID != "987654" {
		t.Errorf("ID = %q", vm.ID)
	}
	if vm.Name != "sandrpod-vm" || vm.Region != "fsn1" || vm.InstanceType != "cx22" {
		t.Errorf("mapping = %+v", vm)
	}
	if vm.State != provider.VMStateRunning {
		t.Errorf("State = %v", vm.State)
	}
	if vm.PublicIP != "1.2.3.4" {
		t.Errorf("PublicIP = %q", vm.PublicIP)
	}
	if !vm.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", vm.CreatedAt, created)
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"CreatedBy":   "CreatedBy",
		"my-sandbox":  "my-sandbox",
		"has space!":  "hasspace",
		"a.b_c-d":     "a.b_c-d",
		"@@@":         "",
		"-leading":    "leading",  // must start alphanumeric
		"trailing-":   "trailing", // must end alphanumeric
		"..dots..":    "dots",
		"@wrapped-@!": "wrapped",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderMetadata(t *testing.T) {
	p := &HetznerProvider{}
	if p.Name() != "hetzner" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DisplayName() != "Hetzner Cloud" {
		t.Errorf("DisplayName() = %q", p.DisplayName())
	}
}

func TestListInstanceTypes_Static(t *testing.T) {
	p := &HetznerProvider{}
	types, err := p.ListInstanceTypes(context.TODO(), "fsn1")
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
