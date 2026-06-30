package aws

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

func TestMapInstanceState(t *testing.T) {
	cases := map[string]provider.VMState{
		"running":       provider.VMStateRunning,
		"pending":       provider.VMStatePending,
		"shutting-down": provider.VMStateStopping,
		"stopping":      provider.VMStateStopping,
		"stopped":       provider.VMStateStopped,
		"terminated":    provider.VMStateStopped,
		"weird-unknown": provider.VMStatePending, // default
	}
	for in, want := range cases {
		if got := mapInstanceState(in); got != want {
			t.Errorf("mapInstanceState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMapEC2ToVMInfo_FullInstance(t *testing.T) {
	launch := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId:       aws.String("i-abc123"),
		InstanceType:     ec2types.InstanceTypeT3Medium,
		PublicIpAddress:  aws.String("1.2.3.4"),
		PrivateIpAddress: aws.String("10.0.0.5"),
		LaunchTime:       aws.Time(launch),
		Placement:        &ec2types.Placement{AvailabilityZone: aws.String("us-east-1a")},
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Tags: []ec2types.Tag{
			{Key: aws.String("sandrpod"), Value: aws.String("true")},
			{Key: aws.String("Name"), Value: aws.String("my-vm")},
		},
	}

	vm := mapEC2ToVMInfo(inst)

	if vm.ID != "i-abc123" {
		t.Errorf("ID = %q", vm.ID)
	}
	if vm.Name != "my-vm" {
		t.Errorf("Name = %q, want my-vm (from Name tag)", vm.Name)
	}
	if vm.PublicIP != "1.2.3.4" || vm.PrivateIP != "10.0.0.5" {
		t.Errorf("IPs = %q / %q", vm.PublicIP, vm.PrivateIP)
	}
	if vm.State != provider.VMStateRunning {
		t.Errorf("State = %v, want running", vm.State)
	}
	if vm.InstanceType != "t3.medium" {
		t.Errorf("InstanceType = %q", vm.InstanceType)
	}
	if !vm.CreatedAt.Equal(launch) {
		t.Errorf("CreatedAt = %v, want %v (from LaunchTime)", vm.CreatedAt, launch)
	}
}

func TestMapEC2ToVMInfo_NilOptionalFields(t *testing.T) {
	inst := ec2types.Instance{
		InstanceId:   aws.String("i-noip"),
		InstanceType: ec2types.InstanceTypeT3Micro,
		Placement:    &ec2types.Placement{AvailabilityZone: aws.String("eu-west-1b")},
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending},
		// no IPs, no LaunchTime, no Name tag
	}

	vm := mapEC2ToVMInfo(inst)

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

func TestProviderMetadata(t *testing.T) {
	p := &AWSProvider{}
	if p.Name() != "aws" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DisplayName() != "Amazon Web Services" {
		t.Errorf("DisplayName() = %q", p.DisplayName())
	}
}

func TestListInstanceTypes_Static(t *testing.T) {
	p := &AWSProvider{}
	types, err := p.ListInstanceTypes(context.TODO(), "us-east-1")
	if err != nil {
		t.Fatalf("ListInstanceTypes: %v", err)
	}
	if len(types) == 0 {
		t.Fatal("expected a non-empty static instance-type list")
	}
	// Every entry must have a name and positive CPU/memory.
	for _, it := range types {
		if it.Name == "" || it.CPU <= 0 || it.MemoryGiB <= 0 {
			t.Errorf("malformed instance type: %+v", it)
		}
	}
}
