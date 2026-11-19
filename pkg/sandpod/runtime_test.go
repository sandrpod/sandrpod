// Copyright 2026 SandrPod Contributors
package sandpod

import (
	"strings"
	"testing"
)

func TestNormalizeSandboxRuntime(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"runc", SandboxRuntimeRunc, false},
		{"RUNC", SandboxRuntimeRunc, false},
		{"gvisor", SandboxRuntimeGVisor, false},
		{"runsc", SandboxRuntimeGVisor, false},
		{"  gVisor  ", SandboxRuntimeGVisor, false},
		{"kata", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeSandboxRuntime(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCapabilityRequirement_GVisor(t *testing.T) {
	got := capabilityRequirement(&CreateSandboxRequest{Runtime: SandboxRuntimeGVisor})
	if len(got) != 1 || got[0] != CapabilityGVisor {
		t.Fatalf("requirements = %v, want [%s]", got, CapabilityGVisor)
	}
	// runc does not require a special worker.
	if got := capabilityRequirement(&CreateSandboxRequest{Runtime: SandboxRuntimeRunc}); len(got) != 0 {
		t.Fatalf("runc requirements = %v, want none", got)
	}
}

func TestCapabilityDescription_GVisor(t *testing.T) {
	if got := capabilityDescription([]string{CapabilityGVisor}); !strings.Contains(got, "gVisor") {
		t.Fatalf("description = %q, want it to name gVisor", got)
	}
}

func TestValidateRuntime_RejectsGPUUnderGVisor(t *testing.T) {
	req := &CreateSandboxRequest{Runtime: "runsc", GPU: 1}
	if err := req.ValidateRuntime(); err == nil || !strings.Contains(err.Error(), "GPU") {
		t.Fatalf("gVisor + GPU must be refused clearly, got %v", err)
	}
	if req.Runtime != SandboxRuntimeGVisor {
		t.Fatalf("runtime should still canonicalize before the rejection, got %q", req.Runtime)
	}
}
