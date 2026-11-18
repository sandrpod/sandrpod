package poder

import (
	"strings"
	"testing"
)

func TestSandboxNetworkNameIsStableAndSandboxScoped(t *testing.T) {
	a := sandboxNetworkName("sb-aaaaaaaaaaaaaaaa")
	b := sandboxNetworkName("sb-bbbbbbbbbbbbbbbb")
	if a != sandboxNetworkName("sb-aaaaaaaaaaaaaaaa") {
		t.Fatal("sandbox network name must be deterministic")
	}
	if a == b {
		t.Fatal("different sandboxes must get different networks")
	}
	if !strings.HasPrefix(a, "sandrpod-s-") || len(a) > 63 {
		t.Fatalf("invalid Docker network name %q", a)
	}
	if strings.Contains(a, "sb-aaaaaaaaaaaaaaaa") {
		t.Fatal("sandbox id must not be exposed in the network name")
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
