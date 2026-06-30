package aliyun

import (
	"testing"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

func TestMapInstanceState(t *testing.T) {
	cases := map[string]provider.VMState{
		"Running":       provider.VMStateRunning,
		"Starting":      provider.VMStatePending,
		"Stopping":      provider.VMStateStopping,
		"Stopped":       provider.VMStateStopped,
		"weird-unknown": provider.VMStatePending, // default
	}
	for in, want := range cases {
		if got := mapInstanceState(in); got != want {
			t.Errorf("mapInstanceState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMapEcsInstanceToVM_FullInstance(t *testing.T) {
	inst := ecs.Instance{
		InstanceId:      "i-abc123",
		InstanceName:    "my-vm",
		RegionId:        "cn-hangzhou",
		InstanceType:    "ecs.t6-c1m1.large",
		Status:          "Running",
		CreationTime:    "2026-01-02T03:04:05Z",
		PublicIpAddress: ecs.PublicIpAddressInDescribeInstances{IpAddress: []string{"1.2.3.4"}},
		InnerIpAddress:  ecs.InnerIpAddressInDescribeInstances{IpAddress: []string{"10.0.0.5"}},
	}

	vm := mapEcsInstanceToVM(inst)

	if vm.ID != "i-abc123" {
		t.Errorf("ID = %q", vm.ID)
	}
	if vm.Name != "my-vm" {
		t.Errorf("Name = %q, want my-vm", vm.Name)
	}
	if vm.PublicIP != "1.2.3.4" || vm.PrivateIP != "10.0.0.5" {
		t.Errorf("IPs = %q / %q", vm.PublicIP, vm.PrivateIP)
	}
	if vm.State != provider.VMStateRunning {
		t.Errorf("State = %v, want running", vm.State)
	}
	if vm.InstanceType != "ecs.t6-c1m1.large" {
		t.Errorf("InstanceType = %q", vm.InstanceType)
	}
	wantTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !vm.CreatedAt.Equal(wantTime) {
		t.Errorf("CreatedAt = %v, want %v (from CreationTime)", vm.CreatedAt, wantTime)
	}
}

func TestMapEcsInstanceToVM_NilOptionalFields(t *testing.T) {
	inst := ecs.Instance{
		InstanceId:   "i-noip",
		InstanceType: "ecs.t6-c1m1.small",
		Status:       "Starting",
		// no IPs, no CreationTime, no InstanceName
	}

	vm := mapEcsInstanceToVM(inst)

	if vm.PublicIP != "" || vm.PrivateIP != "" {
		t.Errorf("expected empty IPs, got %q / %q", vm.PublicIP, vm.PrivateIP)
	}
	if vm.Name != "" {
		t.Errorf("expected empty Name, got %q", vm.Name)
	}
	if !vm.CreatedAt.IsZero() {
		t.Errorf("expected zero CreatedAt, got %v", vm.CreatedAt)
	}
	if vm.State != provider.VMStatePending {
		t.Errorf("State = %v, want pending", vm.State)
	}
}

func TestMapEcsInstanceToVM_MalformedCreationTime(t *testing.T) {
	inst := ecs.Instance{
		InstanceId:   "i-badtime",
		Status:       "Running",
		CreationTime: "not-a-timestamp",
	}

	vm := mapEcsInstanceToVM(inst)

	if !vm.CreatedAt.IsZero() {
		t.Errorf("expected zero CreatedAt for malformed CreationTime, got %v", vm.CreatedAt)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := &AliyunProvider{}
	if p.Name() != "aliyun" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DisplayName() != "Alibaba Cloud" {
		t.Errorf("DisplayName() = %q", p.DisplayName())
	}
}

func TestIsCloudAssistNotReady(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "generic error", err: errPlain("boom"), want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCloudAssistNotReady(c.err); got != c.want {
				t.Errorf("isCloudAssistNotReady(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// errPlain is a minimal error type used to verify isCloudAssistNotReady
// safely handles errors that don't implement the Aliyun SDK error interface.
type errPlain string

func (e errPlain) Error() string { return string(e) }
