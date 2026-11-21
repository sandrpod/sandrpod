package tencent

import (
	"errors"
	"strings"
	"testing"

	sdkerrors "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// Tencent takes a zone where every other provider takes a region; a zone from
// another region fails inside the API with a message about the zone.
func TestZoneFor(t *testing.T) {
	p := &TencentProvider{region: "ap-guangzhou", zone: "ap-guangzhou-3"}

	if got, err := p.zoneFor(""); err != nil || got != "ap-guangzhou-3" {
		t.Errorf("default = %q, %v", got, err)
	}
	if got, err := p.zoneFor("ap-guangzhou-1"); err != nil || got != "ap-guangzhou-1" {
		t.Errorf("explicit = %q, %v", got, err)
	}
	if _, err := p.zoneFor("ap-hongkong-1"); err == nil {
		t.Fatal("a zone from another region was accepted")
	} else if !strings.Contains(err.Error(), "ap-hongkong-1") || !strings.Contains(err.Error(), "ap-guangzhou") {
		t.Errorf("error should name both: %v", err)
	}
	if _, err := (&TencentProvider{}).zoneFor(""); err == nil {
		t.Error("no zone and no default should be an error")
	}
}

// A NAT deployment asks for no public IP and used to get one anyway.
func TestInternetAccessible(t *testing.T) {
	if got := internetAccessible(nil); got == nil || got.PublicIpAssigned == nil || !*got.PublicIpAssigned {
		t.Errorf("default should request a public IP: %+v", got)
	}
	if got := internetAccessible(&provider.NetworkConfig{PublicIP: true}); got == nil {
		t.Error("explicit public IP should request one")
	}
	if got := internetAccessible(&provider.NetworkConfig{PublicIP: false}); got != nil {
		t.Errorf("a private deployment got a public IP: %+v", got)
	}
}

// Spot is a real Tencent mode (SPOTPAID + a one-time market request); without
// it the field must stay empty so the default (POSTPAID_BY_HOUR) applies.
func TestApplySpot(t *testing.T) {
	onDemand := cvm.NewRunInstancesRequest()
	applySpot(onDemand, &provider.CreateVMRequest{})
	if onDemand.InstanceChargeType != nil || onDemand.InstanceMarketOptions != nil {
		t.Fatal("an on-demand create set spot fields")
	}

	spot := cvm.NewRunInstancesRequest()
	applySpot(spot, &provider.CreateVMRequest{Spot: true})
	if spot.InstanceChargeType == nil || *spot.InstanceChargeType != "SPOTPAID" {
		t.Errorf("charge type = %v, want SPOTPAID", spot.InstanceChargeType)
	}
	imo := spot.InstanceMarketOptions
	if imo == nil || imo.MarketType == nil || *imo.MarketType != "spot" ||
		imo.SpotOptions == nil || imo.SpotOptions.SpotInstanceType == nil ||
		*imo.SpotOptions.SpotInstanceType != "one-time" {
		t.Fatalf("market options = %+v", imo)
	}
	if imo.SpotOptions.MaxPrice != nil {
		t.Errorf("no price cap requested but MaxPrice = %v", *imo.SpotOptions.MaxPrice)
	}

	capped := cvm.NewRunInstancesRequest()
	applySpot(capped, &provider.CreateVMRequest{Spot: true, SpotPriceLimit: 0.25})
	if capped.InstanceMarketOptions.SpotOptions.MaxPrice == nil ||
		*capped.InstanceMarketOptions.SpotOptions.MaxPrice != "0.250" {
		t.Errorf("price cap = %v", capped.InstanceMarketOptions.SpotOptions.MaxPrice)
	}
}

// Deleting something already gone is the outcome the caller wanted.
func TestIsNotFound(t *testing.T) {
	if !isNotFound(&sdkerrors.TencentCloudSDKError{Code: "InvalidInstanceId.NotFound"}) {
		t.Error("InvalidInstanceId.NotFound not recognised")
	}
	if isNotFound(&sdkerrors.TencentCloudSDKError{Code: "UnauthorizedOperation"}) {
		t.Error("an unrelated code was treated as NotFound")
	}
	if isNotFound(errors.New("boom")) {
		t.Error("a non-API error was treated as NotFound")
	}
}
