// Copyright 2026 SandrPod Contributors
package main

import "testing"

func TestSplitAddr(t *testing.T) {
	ok := []struct {
		in   string
		host string
		port int
	}{
		{"0.0.0.0:8080", "0.0.0.0", 8080},
		{"127.0.0.1:3128", "127.0.0.1", 3128},
		{"[::]:1080", "[::]", 1080},
	}
	for _, tc := range ok {
		host, port, err := splitAddr(tc.in)
		if err != nil {
			t.Errorf("splitAddr(%q): %v", tc.in, err)
			continue
		}
		if host != tc.host || port != tc.port {
			t.Errorf("splitAddr(%q) = %q,%d; want %q,%d", tc.in, host, port, tc.host, tc.port)
		}
	}
	for _, bad := range []string{"", "8080", "localhost", "localhost:", "localhost:0", "localhost:70000"} {
		if _, _, err := splitAddr(bad); err == nil {
			t.Errorf("splitAddr(%q) should fail", bad)
		}
	}
}

// A proxy with no token would be an open relay on the sandbox's own network.
func TestRun_RequiresToken(t *testing.T) {
	if err := run("127.0.0.1:0", "127.0.0.1:0", "", `{"allowed_domains":[]}`, ""); err == nil {
		t.Fatal("run without a token must fail")
	}
}

func TestRun_RejectsBothPolicySources(t *testing.T) {
	err := run("127.0.0.1:0", "127.0.0.1:0", "tok", `{"allowed_domains":[]}`, "/tmp/x.json")
	if err == nil {
		t.Fatal("giving both -policy-json and -policy must fail")
	}
}
