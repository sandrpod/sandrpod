package tencent

import (
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
	tat "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tat/v20201028"

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
	if err := applySpot(onDemand, &provider.CreateVMRequest{}); err != nil {
		t.Fatalf("on-demand: %v", err)
	}
	if onDemand.InstanceChargeType != nil || onDemand.InstanceMarketOptions != nil {
		t.Fatal("an on-demand create set spot fields")
	}

	// Tencent rejects a spot request without MaxPrice, so a request that does
	// not name a price is refused here with the setting named.
	noPrice := cvm.NewRunInstancesRequest()
	if err := applySpot(noPrice, &provider.CreateVMRequest{Spot: true}); err == nil {
		t.Fatal("spot without a max price was accepted")
	} else if !strings.Contains(err.Error(), "SANDRPOD_SPOT_MAX_PRICE_TENCENT") {
		t.Errorf("error should name the setting: %v", err)
	}

	spot := cvm.NewRunInstancesRequest()
	if err := applySpot(spot, &provider.CreateVMRequest{Spot: true, SpotPriceLimit: 0.05}); err != nil {
		t.Fatalf("applySpot: %v", err)
	}
	if spot.InstanceChargeType == nil || *spot.InstanceChargeType != "SPOTPAID" {
		t.Errorf("charge type = %v, want SPOTPAID", spot.InstanceChargeType)
	}
	imo := spot.InstanceMarketOptions
	if imo == nil || imo.MarketType == nil || *imo.MarketType != "spot" ||
		imo.SpotOptions == nil || imo.SpotOptions.SpotInstanceType == nil ||
		*imo.SpotOptions.SpotInstanceType != "one-time" {
		t.Fatalf("market options = %+v", imo)
	}
	if imo.SpotOptions.MaxPrice == nil || *imo.SpotOptions.MaxPrice != "0.050" {
		t.Errorf("MaxPrice = %v, want 0.050", imo.SpotOptions.MaxPrice)
	}

	capped := cvm.NewRunInstancesRequest()
	_ = applySpot(capped, &provider.CreateVMRequest{Spot: true, SpotPriceLimit: 0.25})
	if capped.InstanceMarketOptions.SpotOptions.MaxPrice == nil ||
		*capped.InstanceMarketOptions.SpotOptions.MaxPrice != "0.250" {
		t.Errorf("price cap = %v", capped.InstanceMarketOptions.SpotOptions.MaxPrice)
	}
}

// TAT kills a command after 60 seconds by default. Every bootstrap command the
// scheduler runs — the Docker install, a cold image pull — outlives that, and the
// provider never said otherwise.
func TestRunCommandSetsATimeout(t *testing.T) {
	if tatCommandTimeout <= time.Minute {
		t.Fatalf("command budget %v is not longer than TAT's 60s default", tatCommandTimeout)
	}
	req := tat.NewRunCommandRequest()
	req.Timeout = common.Uint64Ptr(uint64(tatCommandTimeout.Seconds()))
	if req.Timeout == nil || *req.Timeout < 120 {
		t.Fatalf("timeout = %v, want the command budget", req.Timeout)
	}
}
