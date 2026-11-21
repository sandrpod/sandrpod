package aliyun

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
	"github.com/sandrpod/sandrpod/pkg/provider"
)

// The mapping is the whole feature: without SpotStrategy, RunInstances answers
// with an on-demand machine at the on-demand price.
func TestApplySpot(t *testing.T) {
	onDemand := ecs.CreateRunInstancesRequest()
	applySpot(&provider.CreateVMRequest{}, onDemand)
	if onDemand.SpotStrategy != "" || fmt.Sprint(onDemand.SpotDuration) != "" {
		t.Fatalf("a request without spot set spot fields: %q %v", onDemand.SpotStrategy, onDemand.SpotDuration)
	}

	spot := ecs.CreateRunInstancesRequest()
	applySpot(&provider.CreateVMRequest{Spot: true}, spot)
	if spot.SpotStrategy != "SpotAsPriceGo" {
		t.Errorf("strategy = %q, want SpotAsPriceGo", spot.SpotStrategy)
	}
	if fmt.Sprint(spot.SpotDuration) != "0" {
		t.Errorf("duration = %v, want 0 (no guaranteed period)", spot.SpotDuration)
	}

	capped := ecs.CreateRunInstancesRequest()
	applySpot(&provider.CreateVMRequest{Spot: true, SpotPriceLimit: 0.42}, capped)
	if capped.SpotStrategy != "SpotWithPriceLimit" {
		t.Errorf("strategy = %q, want SpotWithPriceLimit when a bid is given", capped.SpotStrategy)
	}
	if got, err := strconv.ParseFloat(fmt.Sprint(capped.SpotPriceLimit), 64); err != nil || got != 0.42 {
		t.Errorf("price limit = %v, want 0.42", capped.SpotPriceLimit)
	}
}
