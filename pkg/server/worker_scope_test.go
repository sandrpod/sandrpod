package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A worker credential is a machine's. It exists for the tunnel, its own
// heartbeat, the enrolment exchange and the two things an agent must ask
// the control plane directly (capabilities, egress decisions). Anything on the
// sandbox API is an operator or a tenant's call, not a machine's: a compromised
// worker used to be able to create sandboxes on any node in its tenant.
func TestWorkerMayReach(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
		why          string
	}{
		{http.MethodGet, "/ws/poder/connect", true, "the handshake"},
		{http.MethodGet, "/ws/sandbox/connect", true, "the agent handshake"},
		{http.MethodPost, "/api/v1/poders/enroll", true, "invitation becomes a credential"},
		{http.MethodGet, "/api/v1/jobs/poll", false, "no job poll: it returned every tenant's create requests"},
		{http.MethodGet, "/api/v1/capabilities", true, "agents probe before starting"},
		{http.MethodPost, "/api/v1/policy/decide", true, "decider mode asks per connection"},
		{http.MethodPost, "/api/v1/poders/poder-abc/heartbeat", true, "its own lease"},
		{http.MethodGet, "/api/v1/poders/poder-abc/heartbeat", false, "heartbeat is a POST"},
		{http.MethodGet, "/api/v1/sandboxes/my-box/egress", true, "server-mode policy sync"},
		{http.MethodPut, "/api/v1/sandboxes/my-box/egress", false, "a worker does not set policy"},

		// The ones that matter.
		{http.MethodGet, "/api/v1/sandboxes", false, "listing the tenant's sandboxes"},
		{http.MethodPost, "/api/v1/sandboxes", false, "creating one"},
		{http.MethodDelete, "/api/v1/sandboxes/my-box", false, "deleting one"},
		{http.MethodPost, "/api/v1/sandboxes/execute", false, "running code as the tenant"},
		{http.MethodGet, "/api/v1/poders", false, "the fleet view"},
		{http.MethodPost, "/api/v1/poders/poder-abc/sandboxes", false, "pinning a create"},
		{http.MethodPost, "/api/v1/tokens", false, "minting keys"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := workerMayReach(r); got != c.want {
			t.Errorf("%s %s = %v, want %v (%s)", c.method, c.path, got, c.want, c.why)
		}
	}
}
