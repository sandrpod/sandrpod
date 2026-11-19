// Copyright 2026 SandrPod Contributors
package poder

import "testing"

func TestDockerRuntimeName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"runc", "", false},
		{"RUNC", "", false},
		{"gvisor", "runsc", false},
		{"runsc", "runsc", false},
		{"  gVisor ", "runsc", false},
		{"kata", "", true},
	}
	for _, tc := range cases {
		got, err := dockerRuntimeName(tc.in)
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
