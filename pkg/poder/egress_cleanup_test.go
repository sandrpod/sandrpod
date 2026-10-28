package poder

import "testing"

// The names are the contract: cleanup must work without the in-memory map, which
// is empty after a restart.
func TestEgressCleanupTargets_KnownNames(t *testing.T) {
	sidecar, network := egressCleanupTargets(nil, "sp-1")
	if sidecar != "sandrpod-egress-proxy-sp-1" {
		t.Fatalf("sidecar = %q", sidecar)
	}
	if network != "sandrpod-egress-sp-1" {
		t.Fatalf("network = %q", network)
	}
}

// A recorded entry wins, so a deployment that ever changes the naming still
// cleans up what it actually created.
func TestEgressCleanupTargets_RecordedWins(t *testing.T) {
	e := &podEgress{sidecar: "custom-proxy", network: "custom-net"}
	sidecar, network := egressCleanupTargets(e, "sp-1")
	if sidecar != "custom-proxy" || network != "custom-net" {
		t.Fatalf("got %q / %q", sidecar, network)
	}
}

// An entry with empty fields (a partially built record) must not erase the
// deterministic names, or the objects would never be found again.
func TestEgressCleanupTargets_EmptyFieldsFallBack(t *testing.T) {
	sidecar, network := egressCleanupTargets(&podEgress{}, "sp-2")
	if sidecar != "sandrpod-egress-proxy-sp-2" || network != "sandrpod-egress-sp-2" {
		t.Fatalf("got %q / %q", sidecar, network)
	}
}
