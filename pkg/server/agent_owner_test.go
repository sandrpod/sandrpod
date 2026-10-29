package server

import "testing"

// A record's owner decides who can reach the machine. A tenant-bearing credential
// (a per-workspace join token included) must produce the same shape the portal
// filters on, not the literal name "worker" — which would have made every tenant's
// agent share one owner.
func TestSandboxOwner(t *testing.T) {
	cases := []struct {
		name  string
		ident Identity
		want  string
	}{
		{"join token", Identity{Name: "worker", Role: RoleWorker, Tenant: "ws-1"}, "workspace:ws-1"},
		{"user token", Identity{Name: "alice", Role: RoleUser, Tenant: "ws-1"}, "workspace:ws-1"},
		{"admin, self-hosted", Identity{Name: "admin", Role: RoleAdmin}, "admin"},
		{"auth disabled", Identity{}, ""},
	}
	for _, c := range cases {
		if got := sandboxOwner(c.ident); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
