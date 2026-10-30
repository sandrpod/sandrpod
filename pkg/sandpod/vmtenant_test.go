package sandpod_test

import (
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// A provisioned VM's worker dials in with the platform token, which carries no
// tenant. The scheduler records who asked; registration takes it — and keeps taking
// it, because a worker reconnects after every control-plane restart.
func TestVMTenant_SurvivesReconnects(t *testing.T) {
	podpkg.RecordVMTenant("vm-1", "workspace-a")
	for i := 0; i < 3; i++ {
		got, ok := podpkg.VMTenant("vm-1")
		if !ok || got != "workspace-a" {
			t.Fatalf("registration %d: got %q,%v want workspace-a,true", i, got, ok)
		}
	}
}

// Nothing recorded, nothing invented: a self-hosted fleet stays unscoped.
func TestVMTenant_UnknownIsEmpty(t *testing.T) {
	if got, ok := podpkg.VMTenant("vm-never-seen"); ok || got != "" {
		t.Fatalf("got %q,%v", got, ok)
	}
	podpkg.RecordVMTenant("", "workspace-a")
	podpkg.RecordVMTenant("vm-2", "")
	if _, ok := podpkg.VMTenant("vm-2"); ok {
		t.Fatal("an empty tenant was recorded")
	}
}
