package server

import "testing"

// The request URL may name the sandbox by its workspace-scoped display name
// while the handler has already resolved it to the routing id. The toolbox
// sub-path must survive that mismatch: matching the resolved key against the
// unresolved path dropped it, turning POST /files/upload into POST /files.
func TestToolboxSubPathDisplayNameVsRoutingKey(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		sandbox  string
		expected string
	}{
		{
			name:     "display name in path, routing id resolved",
			path:     "/api/v1/sandboxes/web-dev/toolbox/files/upload",
			sandbox:  "sb-0123456789abcdef",
			expected: "files/upload",
		},
		{
			name:     "routing id in path",
			path:     "/api/v1/sandboxes/sb-0123456789abcdef/toolbox/files/folder",
			sandbox:  "sb-0123456789abcdef",
			expected: "files/folder",
		},
		{
			name:     "nested proxy path",
			path:     "/api/v1/sandboxes/web-dev/toolbox/proxy/8000/",
			sandbox:  "sb-0123456789abcdef",
			expected: "proxy/8000/",
		},
		{
			name:     "toolbox root falls back to files",
			path:     "/api/v1/sandboxes/web-dev/toolbox",
			sandbox:  "sb-0123456789abcdef",
			expected: "files",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolboxSubPath(tc.path, tc.sandbox); got != tc.expected {
				t.Fatalf("toolboxSubPath(%q, %q) = %q, want %q",
					tc.path, tc.sandbox, got, tc.expected)
			}
		})
	}
}
