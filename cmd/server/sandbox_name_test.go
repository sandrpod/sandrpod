package main

import (
	"strings"
	"testing"
)

// A name is a URL path segment. Nothing downstream rejects a bad one — the
// container is named by a generated id — so a sandbox called "a/b" or "" was
// created, ran, and could never be fetched or deleted again through the API.
// It had to be removed with docker and SQL.
func TestValidateSandboxName(t *testing.T) {
	unaddressable := []struct{ name, why string }{
		{"", "empty: /api/v1/sandboxes/ is a different route"},
		{"a/b", "a slash splits into name + action"},
		{"../../etc-x", "traversal shape, and slashes again"},
		{"/leading", "leading slash"},
		{"trailing/", "trailing slash"},
		{strings.Repeat("a", 65), "beyond the length bound"},
		{"-startswithdash", "must start alphanumeric"},
		{"has space", "space needs encoding everywhere it appears"},
	}
	for _, tc := range unaddressable {
		if validateSandboxName(tc.name) == "" {
			t.Errorf("accepted %q — %s", tc.name, tc.why)
		}
	}

	for _, ok := range []string{
		"a", "sandbox-1", "my_box", "box.v2", "A1",
		strings.Repeat("a", 64), // exactly at the bound
	} {
		if reason := validateSandboxName(ok); reason != "" {
			t.Errorf("rejected %q: %s", ok, reason)
		}
	}
}
