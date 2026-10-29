package server

import (
	"net/http"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// A worker on a provisioned VM dials in with the platform token, which carries no
// tenant. It must inherit the workspace that paid for the machine — otherwise it is
// invisible in that workspace and eligible for every other one.
func TestPoderWorkspace_InheritsFromProvisioningCreate(t *testing.T) {
	podpkg.RecordVMTenant("vm-abc", "workspace-a")

	r, _ := http.NewRequest(http.MethodGet, "/ws/poder/connect", nil)
	r.Header.Set("X-Poder-ID", "poder-1")
	r.Header.Set("X-Poder-VM-ID", "vm-abc")

	if got := poderWorkspaceOrProvisioner(r); got != "workspace-a" {
		t.Fatalf("workspace = %q, want workspace-a", got)
	}
}

// An explicit header or a tenant-bearing credential still wins: the record only
// fills a gap.
func TestPoderWorkspace_ExplicitHeaderWins(t *testing.T) {
	podpkg.RecordVMTenant("vm-def", "workspace-from-record")

	r, _ := http.NewRequest(http.MethodGet, "/ws/poder/connect", nil)
	r.Header.Set("X-Poder-Workspace", "workspace-header")
	r.Header.Set("X-Poder-VM-ID", "vm-def")

	if got := poderWorkspaceOrProvisioner(r); got != "workspace-header" {
		t.Fatalf("workspace = %q, want workspace-header", got)
	}
}

// No record and no header: a self-hosted fleet stays unscoped, which is what makes
// it usable by any workspace on that server.
func TestPoderWorkspace_UnscopedByDefault(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/ws/poder/connect", nil)
	r.Header.Set("X-Poder-VM-ID", "vm-never-provisioned")
	if got := poderWorkspaceOrProvisioner(r); got != "" {
		t.Fatalf("workspace = %q, want empty", got)
	}
}
