package sandpod_test

import (
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// A provisioned VM's worker dials in with the platform token, which carries no
// tenant. The scheduler records who asked; registration takes it once.
func TestVMTenant_RecordedAndTakenOnce(t *testing.T) {
	podpkg.RecordVMTenant("vm-1", "workspace-a")
	if got, ok := podpkg.TakeVMTenant("vm-1"); !ok || got != "workspace-a" {
		t.Fatalf("got %q,%v want workspace-a,true", got, ok)
	}
	// Consumed: a second worker must not silently inherit the same tenant.
	if _, ok := podpkg.TakeVMTenant("vm-1"); ok {
		t.Fatal("the record was not consumed")
	}
}

// Nothing recorded, nothing invented: a self-hosted fleet stays unscoped.
func TestVMTenant_UnknownIsEmpty(t *testing.T) {
	if got, ok := podpkg.TakeVMTenant("vm-never-seen"); ok || got != "" {
		t.Fatalf("got %q,%v", got, ok)
	}
	podpkg.RecordVMTenant("", "workspace-a")
	podpkg.RecordVMTenant("vm-2", "")
	if _, ok := podpkg.TakeVMTenant("vm-2"); ok {
		t.Fatal("an empty tenant was recorded")
	}
}
