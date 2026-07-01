package tencent

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	sdkerrors "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

func TestMapState(t *testing.T) {
	cases := map[string]provider.VMState{
		"RUNNING":       provider.VMStateRunning,
		"PENDING":       provider.VMStatePending,
		"STARTING":      provider.VMStatePending,
		"STOPPING":      provider.VMStateStopping,
		"STOPPED":       provider.VMStateStopped,
		"LAUNCH_FAILED": provider.VMStateError,
		"weird":         provider.VMStatePending,
	}
	for in, want := range cases {
		if got := mapState(in); got != want {
			t.Errorf("mapState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMapInstance(t *testing.T) {
	inst := &cvm.Instance{
		InstanceId:         common.StringPtr("ins-abc123"),
		InstanceName:       common.StringPtr("sandrpod-vm"),
		InstanceType:       common.StringPtr("S5.MEDIUM4"),
		InstanceState:      common.StringPtr("RUNNING"),
		CreatedTime:        common.StringPtr("2026-01-02T03:04:05Z"),
		Placement:          &cvm.Placement{Zone: common.StringPtr("ap-guangzhou-3")},
		PublicIpAddresses:  []*string{common.StringPtr("1.2.3.4")},
		PrivateIpAddresses: []*string{common.StringPtr("10.0.0.5")},
	}
	vm := mapInstance(inst)
	if vm.ID != "ins-abc123" || vm.Name != "sandrpod-vm" || vm.InstanceType != "S5.MEDIUM4" {
		t.Errorf("mapping = %+v", vm)
	}
	if vm.Region != "ap-guangzhou-3" {
		t.Errorf("Region = %q", vm.Region)
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

func TestMapInstance_NilSafe(t *testing.T) {
	vm := mapInstance(&cvm.Instance{InstanceId: common.StringPtr("ins-bare"), InstanceState: common.StringPtr("PENDING")})
	if vm.PublicIP != "" || vm.PrivateIP != "" || vm.Region != "" {
		t.Errorf("expected empty optional fields, got %+v", vm)
	}
	if !vm.CreatedAt.IsZero() {
		t.Errorf("expected zero CreatedAt, got %v", vm.CreatedAt)
	}
	if vm.State != provider.VMStatePending {
		t.Errorf("State = %v", vm.State)
	}
}

func TestIsAgentNotReady(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"agent code", sdkerrors.NewTencentCloudSDKError("ResourceUnavailable.AgentStatusNotOnline", "not online", ""), true},
		{"other code", sdkerrors.NewTencentCloudSDKError("InvalidParameterValue", "bad", ""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAgentNotReady(c.err); got != c.want {
				t.Errorf("isAgentNotReady(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestProviderMetadata(t *testing.T) {
	p := &TencentProvider{}
	if p.Name() != "tencent" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DisplayName() != "Tencent Cloud" {
		t.Errorf("DisplayName() = %q", p.DisplayName())
	}
}

func TestListInstanceTypes_Static(t *testing.T) {
	p := &TencentProvider{}
	types, err := p.ListInstanceTypes(context.TODO(), "ap-guangzhou")
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
