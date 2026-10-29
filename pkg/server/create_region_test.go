package server

import (
	"strings"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// A cloud provider given "local" used to reach the provider SDK, which turned it
// into ecs.local.aliyuncs.com and answered a DNS error. It has to be refused here,
// where the message can say what is wrong.
func TestCreate_CloudProviderNeedsRealRegion(t *testing.T) {
	for _, tc := range []struct{ provider, region string }{
		{"aliyun", "local"},
		{"aws", "local"},
		{"aliyun", ""},
	} {
		stores := store.NewMemoryStores()
		req := &podpkg.CreateSandboxRequest{Name: "bad-region", ProviderType: tc.provider, Region: tc.region}
		_, _, err := runSandboxCreate(nil, stores.Sandboxes, stores.Poders, stores.Jobs,
			tunnel.NewTunnelStore(), Config{}, stores.TunnelOwners, req, "", "owner", nil, Identity{Name: "owner"})
		if err == nil {
			t.Fatalf("%s/%q: expected a refusal", tc.provider, tc.region)
		}
		if !strings.Contains(err.Error(), "needs a region") {
			t.Fatalf("%s/%q: unhelpful error %q", tc.provider, tc.region, err)
		}
	}
}

// The local worker fleet keeps using "local": it is this control plane's own word,
// and the guard must not touch it.
func TestIsCloudProviderName(t *testing.T) {
	for _, local := range []string{"", "local", "local-agent", "agent", "docker"} {
		if isCloudProviderName(local) {
			t.Errorf("%q treated as a cloud provider", local)
		}
	}
	for _, cloud := range []string{"aws", "aliyun", "azure", "gcp", "tencent", "digitalocean", "hetzner", "oracle"} {
		if !isCloudProviderName(cloud) {
			t.Errorf("%q treated as local", cloud)
		}
	}
}

// The refusal happens before scheduling, so no job is left behind for the resumer
// to deliver later.
func TestCreate_RegionRefusalLeavesNoJob(t *testing.T) {
	stores := store.NewMemoryStores()
	req := &podpkg.CreateSandboxRequest{Name: "no-job", ProviderType: "aliyun", Region: "local"}
	_, _, _ = runSandboxCreate(nil, stores.Sandboxes, stores.Poders, stores.Jobs,
		tunnel.NewTunnelStore(), Config{}, stores.TunnelOwners, req, "", "owner", nil, Identity{Name: "owner"})
	if jobs := stores.Jobs.ListJobs(); len(jobs) != 0 {
		t.Fatalf("a refused create left %d job(s) behind", len(jobs))
	}
}
