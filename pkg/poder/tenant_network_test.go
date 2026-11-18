package poder

import (
	"strings"
	"testing"
)

func TestTenantNetworkNameIsStableAndTenantScoped(t *testing.T) {
	a := tenantNetworkName("workspace-a")
	b := tenantNetworkName("workspace-b")
	if a != tenantNetworkName("workspace-a") {
		t.Fatal("tenant network name must be deterministic")
	}
	if a == b {
		t.Fatal("different tenants must get different networks")
	}
	if !strings.HasPrefix(a, "sandrpod-t-") || len(a) > 63 {
		t.Fatalf("invalid Docker network name %q", a)
	}
	if strings.Contains(a, "workspace-a") {
		t.Fatal("tenant id must not be exposed in the network name")
	}
}

func TestPidsLimitDefaultAndOverride(t *testing.T) {
	t.Setenv("SANDRPOD_PIDS_LIMIT", "")
	if got := pidsLimit(); got != 2048 {
		t.Fatalf("default pids limit = %d, want 2048", got)
	}
	t.Setenv("SANDRPOD_PIDS_LIMIT", "0")
	if got := pidsLimit(); got != 0 {
		t.Fatalf("explicit zero pids limit = %d, want 0", got)
	}
	t.Setenv("SANDRPOD_PIDS_LIMIT", "2048")
	if got := pidsLimit(); got != 2048 {
		t.Fatalf("pids limit override = %d, want 2048", got)
	}
}
