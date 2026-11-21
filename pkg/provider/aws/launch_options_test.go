package aws

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// apiErr is a minimal smithy.APIError for the classifier tests.
type apiErr struct{ code string }

func (e apiErr) Error() string                 { return e.code }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.code }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestApplyLaunchOptions_Spot(t *testing.T) {
	onDemand := &ec2.RunInstancesInput{}
	applyLaunchOptions(onDemand, &provider.CreateVMRequest{})
	if onDemand.InstanceMarketOptions != nil {
		t.Fatal("an on-demand create asked for spot capacity")
	}

	spot := &ec2.RunInstancesInput{}
	applyLaunchOptions(spot, &provider.CreateVMRequest{Spot: true})
	imo := spot.InstanceMarketOptions
	if imo == nil || imo.MarketType != ec2types.MarketTypeSpot {
		t.Fatalf("market options = %+v", imo)
	}
	if imo.SpotOptions == nil ||
		imo.SpotOptions.SpotInstanceType != ec2types.SpotInstanceTypeOneTime ||
		imo.SpotOptions.InstanceInterruptionBehavior != ec2types.InstanceInterruptionBehaviorTerminate {
		t.Errorf("spot options = %+v, want one-time + terminate", imo.SpotOptions)
	}
}

// Sandboxes are containers on this host: anything that can reach the metadata
// endpoint can read the instance role's credentials.
func TestApplyLaunchOptions_RequiresIMDSv2(t *testing.T) {
	in := &ec2.RunInstancesInput{}
	applyLaunchOptions(in, &provider.CreateVMRequest{})
	mo := in.MetadataOptions
	if mo == nil {
		t.Fatal("no metadata options: IMDSv1 stays available to every container")
	}
	if mo.HttpTokens != ec2types.HttpTokensStateRequired {
		t.Errorf("HttpTokens = %q, want required", mo.HttpTokens)
	}
	if mo.HttpPutResponseHopLimit != nil && *mo.HttpPutResponseHopLimit != 1 {
		t.Errorf("hop limit = %d, want 1 so a container cannot proxy a token", *mo.HttpPutResponseHopLimit)
	}
}

// A delete of something already gone is the outcome the caller wanted; an
// orphan-cleanup message saying "left running" about a terminated machine sends
// someone to the console for nothing.
func TestNotFoundClassification(t *testing.T) {
	if !isNotFound(apiErr{code: "InvalidInstanceID.NotFound"}) {
		t.Error("InvalidInstanceID.NotFound not recognised")
	}
	if !isNotFound(fmt.Errorf("wrapped: %w", apiErr{code: "InvalidInstanceId.NotFound"})) {
		t.Error("wrapped NotFound not recognised")
	}
	if isNotFound(apiErr{code: "UnauthorizedOperation"}) || isNotFound(errors.New("network down")) {
		t.Error("unrelated errors classified as NotFound")
	}
}

// "Not registered yet" is polled through; everything else is the caller's error.
func TestInvocationNotReadyClassification(t *testing.T) {
	for _, code := range []string{"InvocationDoesNotExist", "InvalidInstanceId"} {
		if !isInvocationNotReady(apiErr{code: code}) {
			t.Errorf("%s should be retried", code)
		}
	}
	for _, code := range []string{"AccessDeniedException", "ThrottlingException"} {
		if isInvocationNotReady(apiErr{code: code}) {
			t.Errorf("%s should surface, not be polled through", code)
		}
	}
	if isInvocationNotReady(errors.New("boom")) {
		t.Error("a non-API error was treated as retryable")
	}
}

var _ = aws.Int32
