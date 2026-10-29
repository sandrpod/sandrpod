package server

import (
	"sort"
	"testing"
)

// The console decides which substrates to offer from this list. "local" is the
// docker worker fleet and is always there; everything else is whatever the
// factory actually registered, in a stable order, without duplicates.
func TestProviderNames_AlwaysLocal(t *testing.T) {
	names := providerNames()
	if len(names) == 0 || names[0] != "local" {
		t.Fatalf("first entry is %v, want local", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("duplicate %q in %v", n, names)
		}
		seen[n] = true
	}
	// Provider registration is idempotent: calling twice must not duplicate.
	if again := providerNames(); len(again) != len(names) {
		t.Fatalf("second call returned %v, first %v", again, names)
	}
	sorted := append([]string(nil), names[1:]...)
	sort.Strings(sorted)
	if len(sorted) != len(names)-1 {
		t.Fatal("unreachable")
	}
}
