package gcp

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

func TestMapGCPState(t *testing.T) {
	cases := map[string]provider.VMState{
		"RUNNING":       provider.VMStateRunning,
		"PROVISIONING":  provider.VMStatePending,
		"STAGING":       provider.VMStatePending,
		"STOPPING":      provider.VMStateStopping,
		"TERMINATED":    provider.VMStateStopped,
		"SUSPENDED":     provider.VMStateStopped,
		"weird-unknown": provider.VMStatePending, // default
	}
	for in, want := range cases {
		if got := mapGCPState(in); got != want {
			t.Errorf("mapGCPState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLastSegment(t *testing.T) {
	cases := map[string]string{
		"https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/machineTypes/e2-medium": "e2-medium",
		"zones/us-central1-a": "us-central1-a",
		"no-slashes":          "no-slashes",
		"":                    "",
		"trailing/":           "",
	}
	for in, want := range cases {
		if got := lastSegment(in); got != want {
			t.Errorf("lastSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"CreatedBy":   "createdby",
		"my-sandbox":  "my-sandbox",
		"Has Spaces!": "hasspaces",
		"under_score": "under_score",
		"@@@":         "",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeLabel_Truncates(t *testing.T) {
	got := sanitizeLabel(strings.Repeat("a", 100))
	if len(got) != 63 {
		t.Errorf("sanitizeLabel truncation len = %d, want 63", len(got))
	}
}

func TestBuildLabels(t *testing.T) {
	labels := buildLabels(map[string]string{"CreatedBy": "sandrpod", "Sandbox": "my-vm", "@@@": "bad"})
	if labels[labelKey] != "true" {
		t.Errorf("missing sandrpod marker label: %v", labels)
	}
	if labels["createdby"] != "sandrpod" {
		t.Errorf("expected sanitized createdby label, got %v", labels)
	}
	if labels["sandbox"] != "my-vm" {
		t.Errorf("expected sanitized sandbox label, got %v", labels)
	}
	// A key that sanitizes to empty must be dropped, not stored under "".
	if _, ok := labels[""]; ok {
		t.Errorf("empty label key should be dropped: %v", labels)
	}
}

func TestGenerateSSHKey(t *testing.T) {
	signer, authKey, err := generateSSHKey("sandrpod")
	if err != nil {
		t.Fatalf("generateSSHKey: %v", err)
	}
	if signer == nil {
		t.Fatal("nil signer")
	}
	if !strings.HasPrefix(authKey, "ssh-ed25519 ") {
		t.Errorf("authorized key should be ed25519, got %q", authKey)
	}
	if !strings.Contains(authKey, "sandrpod-sandrpod") {
		t.Errorf("authorized key should carry an identifying comment, got %q", authKey)
	}
	// Two calls must produce distinct keys (ephemeral, per-VM).
	_, authKey2, _ := generateSSHKey("sandrpod")
	if authKey == authKey2 {
		t.Error("expected distinct ephemeral keys across calls")
	}
}

func TestMapInstanceToVM(t *testing.T) {
	inst := &computepb.Instance{
		Name:              strPtr("sandrpod-vm"),
		Zone:              strPtr("https://compute/projects/p/zones/us-central1-a"),
		MachineType:       strPtr("https://compute/projects/p/zones/us-central1-a/machineTypes/e2-medium"),
		Status:            strPtr("RUNNING"),
		CreationTimestamp: strPtr("2026-01-02T03:04:05Z"),
		NetworkInterfaces: []*computepb.NetworkInterface{{
			NetworkIP:     strPtr("10.0.0.5"),
			AccessConfigs: []*computepb.AccessConfig{{NatIP: strPtr("1.2.3.4")}},
		}},
	}
	vm := mapInstanceToVM(inst)
	if vm.ID != "sandrpod-vm" || vm.Name != "sandrpod-vm" {
		t.Errorf("name = %q", vm.Name)
	}
	if vm.Region != "us-central1-a" {
		t.Errorf("region = %q (want zone last segment)", vm.Region)
	}
	if vm.InstanceType != "e2-medium" {
		t.Errorf("instanceType = %q", vm.InstanceType)
	}
	if vm.State != provider.VMStateRunning {
		t.Errorf("state = %v", vm.State)
	}
	if vm.PublicIP != "1.2.3.4" || vm.PrivateIP != "10.0.0.5" {
		t.Errorf("IPs = %q / %q", vm.PublicIP, vm.PrivateIP)
	}
	if vm.CreatedAt.IsZero() {
		t.Error("expected parsed CreatedAt")
	}
}

func TestMapInstanceToVM_MinimalFields(t *testing.T) {
	vm := mapInstanceToVM(&computepb.Instance{Name: strPtr("bare"), Status: strPtr("PROVISIONING")})
	if vm.PublicIP != "" || vm.PrivateIP != "" {
		t.Errorf("expected empty IPs, got %q / %q", vm.PublicIP, vm.PrivateIP)
	}
	if !vm.CreatedAt.IsZero() {
		t.Errorf("expected zero CreatedAt, got %v", vm.CreatedAt)
	}
	if vm.State != provider.VMStatePending {
		t.Errorf("state = %v", vm.State)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := &GCPProvider{}
	if p.Name() != "gcp" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DisplayName() != "Google Cloud Platform" {
		t.Errorf("DisplayName() = %q", p.DisplayName())
	}
}

func TestListInstanceTypes_Static(t *testing.T) {
	p := &GCPProvider{}
	types, err := p.ListInstanceTypes(context.TODO(), "us-central1")
	if err != nil {
		t.Fatalf("ListInstanceTypes: %v", err)
	}
	if len(types) == 0 {
		t.Fatal("expected a non-empty static instance-type list")
	}
	for _, it := range types {
		if it.Name == "" || it.CPU <= 0 || it.MemoryGiB <= 0 {
			t.Errorf("malformed instance type: %+v", it)
		}
	}
}
