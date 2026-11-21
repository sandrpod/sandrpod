package aws

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// Only Success is a success. A non-Success terminal status is a failure whatever
// code it carries: depending on ResponseCode there is one AWS version away from a
// machine whose docker install never finished reading as exit 0.
func TestCommandResult_TerminalStatuses(t *testing.T) {
	inv := func(status ssmtypes.CommandInvocationStatus, code int32, out, errOut string) *ssm.GetCommandInvocationOutput {
		return &ssm.GetCommandInvocationOutput{
			Status:                status,
			ResponseCode:          code,
			StandardOutputContent: aws.String(out),
			StandardErrorContent:  aws.String(errOut),
		}
	}

	if got := commandResult(inv(ssmtypes.CommandInvocationStatusSuccess, 0, "ok", ""), "c1"); got.ExitCode != 0 || got.Stderr != "" {
		t.Errorf("success = %+v, want exit 0", got)
	}
	if got := commandResult(inv(ssmtypes.CommandInvocationStatusSuccess, 3, "", "boom"), "c1"); got.ExitCode != 3 {
		t.Errorf("success with a non-zero code = %d, want 3", got.ExitCode)
	}
	for _, st := range []ssmtypes.CommandInvocationStatus{
		ssmtypes.CommandInvocationStatusTimedOut,
		ssmtypes.CommandInvocationStatusCancelled,
		ssmtypes.CommandInvocationStatusFailed,
	} {
		got := commandResult(inv(st, 0, "", ""), "c9")
		if got.ExitCode == 0 {
			t.Errorf("%s reported exit 0 — the scheduler would treat it as success", st)
		}
		if !strings.Contains(got.Stderr, string(st)) {
			t.Errorf("%s: stderr %q does not name the status", st, got.Stderr)
		}
	}
}

// The region the request names has to be the one the clients are bound to,
// otherwise the VM lands somewhere else while the record says otherwise.
func TestCheckRegion(t *testing.T) {
	p := &AWSProvider{region: "us-east-1"}
	if err := p.checkRegion(""); err != nil {
		t.Errorf("empty region should mean the configured one: %v", err)
	}
	if err := p.checkRegion("us-east-1"); err != nil {
		t.Errorf("matching region refused: %v", err)
	}
	err := p.checkRegion("eu-west-1")
	if err == nil {
		t.Fatal("a mismatched region was accepted")
	}
	if !strings.Contains(err.Error(), "eu-west-1") || !strings.Contains(err.Error(), "us-east-1") {
		t.Errorf("error should name both regions: %v", err)
	}
}

// A public IP rides on a network interface, and an interface without a subnet is
// rejected by the API with a message that reads like a code bug.
func TestNetworkInput(t *testing.T) {
	// No config at all: AWS's own defaults.
	if ni, subnet, sg, err := networkInput(nil); ni != nil || subnet != "" || sg != nil || err != nil {
		t.Errorf("nil config = %v %q %v %v", ni, subnet, sg, err)
	}

	// Public IP without a subnet is the trap.
	_, _, _, err := networkInput(&provider.NetworkConfig{PublicIP: true})
	if err == nil {
		t.Fatal("public IP without a subnet was accepted")
	}
	if !strings.Contains(err.Error(), "SANDRPOD_VM_SUBNET_ID") {
		t.Errorf("error should name the setting: %v", err)
	}

	// With a subnet, the interface carries subnet and group.
	ni, subnet, sg, err := networkInput(&provider.NetworkConfig{PublicIP: true, SubnetID: "subnet-1", SecurityGroup: "sg-1"})
	if err != nil || ni == nil || subnet != "" || sg != nil {
		t.Fatalf("public+subnet = %v %q %v %v", ni, subnet, sg, err)
	}
	if aws.ToString(ni.SubnetId) != "subnet-1" || len(ni.Groups) != 1 || ni.Groups[0] != "sg-1" {
		t.Errorf("interface = %+v", ni)
	}
	if ni.AssociatePublicIpAddress == nil || !*ni.AssociatePublicIpAddress {
		t.Error("public IP not requested on the interface")
	}

	// Without a public IP the top-level fields are used — the combination AWS
	// forbids mixing.
	ni, subnet, sg, err = networkInput(&provider.NetworkConfig{SubnetID: "subnet-2", SecurityGroup: "sg-2"})
	if err != nil || ni != nil || subnet != "subnet-2" || len(sg) != 1 || sg[0] != "sg-2" {
		t.Fatalf("private = %v %q %v %v", ni, subnet, sg, err)
	}
}

// A token per call: retries of this call reuse it, a later create does not.
func TestNewClientToken(t *testing.T) {
	a, b := newClientToken(), newClientToken()
	if a == b {
		t.Fatal("two creates got the same token — they would be deduplicated")
	}
	if !strings.HasPrefix(a, "sandrpod-") || len(a) > 64 {
		t.Errorf("token %q is not the expected shape", a)
	}
}
