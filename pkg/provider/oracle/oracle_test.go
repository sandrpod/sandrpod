package oracle

import (
	"context"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

func TestMapState(t *testing.T) {
	cases := map[core.InstanceLifecycleStateEnum]provider.VMState{
		core.InstanceLifecycleStateRunning:      provider.VMStateRunning,
		core.InstanceLifecycleStateProvisioning: provider.VMStatePending,
		core.InstanceLifecycleStateStarting:     provider.VMStatePending,
		core.InstanceLifecycleStateStopping:     provider.VMStateStopping,
		core.InstanceLifecycleStateTerminating:  provider.VMStateStopping,
		core.InstanceLifecycleStateStopped:      provider.VMStateStopped,
		core.InstanceLifecycleStateTerminated:   provider.VMStateStopped,
	}
	for in, want := range cases {
		if got := mapState(in); got != want {
			t.Errorf("mapState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMapInstance(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	inst := core.Instance{
		Id:             common.String("ocid1.instance.oc1..abc"),
		DisplayName:    common.String("sandrpod-vm"),
		Region:         common.String("us-ashburn-1"),
		Shape:          common.String("VM.Standard.A1.Flex"),
		LifecycleState: core.InstanceLifecycleStateRunning,
		TimeCreated:    &common.SDKTime{Time: created},
	}
	vm := mapInstance(inst)
	if vm.ID != "ocid1.instance.oc1..abc" || vm.Name != "sandrpod-vm" {
		t.Errorf("mapping = %+v", vm)
	}
	if vm.Region != "us-ashburn-1" || vm.InstanceType != "VM.Standard.A1.Flex" {
		t.Errorf("mapping = %+v", vm)
	}
	if vm.State != provider.VMStateRunning {
		t.Errorf("State = %v", vm.State)
	}
	if !vm.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", vm.CreatedAt, created)
	}
}

func TestMapInstance_NilSafe(t *testing.T) {
	vm := mapInstance(core.Instance{LifecycleState: core.InstanceLifecycleStateProvisioning})
	if vm.ID != "" || vm.Name != "" || vm.Region != "" {
		t.Errorf("expected empty fields, got %+v", vm)
	}
	if !vm.CreatedAt.IsZero() {
		t.Errorf("expected zero CreatedAt, got %v", vm.CreatedAt)
	}
	if vm.State != provider.VMStatePending {
		t.Errorf("State = %v", vm.State)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := &OracleProvider{}
	if p.Name() != "oracle" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DisplayName() != "Oracle Cloud Infrastructure" {
		t.Errorf("DisplayName() = %q", p.DisplayName())
	}
}

func TestListInstanceTypes_Static(t *testing.T) {
	p := &OracleProvider{}
	types, err := p.ListInstanceTypes(context.TODO(), "us-ashburn-1")
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
