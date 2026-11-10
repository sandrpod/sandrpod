// Copyright 2026 SandrPod Contributors
package sandpod

import (
	"testing"

	"github.com/sandrpod/sandrpod/pkg/egress"
)

// A GPU sandbox must never be placed on a worker without the capability: the
// alternative is a CUDA job that fails inside the container, which reads like a
// platform bug rather than a placement refusal.
func TestCapabilityRequirementGPU(t *testing.T) {
	cases := []struct {
		name string
		req  *CreateSandboxRequest
		want []string
	}{
		{"plain", &CreateSandboxRequest{}, nil},
		{"gpu", &CreateSandboxRequest{GPU: 2}, []string{CapabilityGPU}},
		{"gpu+egress", &CreateSandboxRequest{GPU: 1, Network: &egress.Policy{}}, []string{CapabilityEgress, CapabilityGPU}},
	}
	for _, c := range cases {
		got := capabilityRequirement(c.req)
		if len(got) != len(c.want) {
			t.Fatalf("%s: require = %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: require = %v, want %v", c.name, got, c.want)
			}
		}
	}
}
