// Copyright 2026 SandrPod Contributors
// What a worker credential may reach.

package server

import (
	"net/http"
	"strings"
)

// workerMayReach lists the routes a machine's credential exists for. A worker is
// a poder (the tunnel handshake, the job poll, its own heartbeat) or an agent
// (the handshake, the capability probe, the egress policy it must enforce). None
// of them manages sandboxes over HTTP: a poder acts on a job, and an agent acts
// on what arrives through its tunnel.
//
// The list is deliberately explicit. The alternative — refusing a handful of
// paths — fails open every time a route is added, and the route that gets added
// is usually the one that can spend the operator's money.
func workerMayReach(r *http.Request) bool {
	switch r.URL.Path {
	case "/ws/poder/connect", "/ws/sandbox/connect":
		return true
	case "/api/v1/poders/enroll":
		// The invitation is presented before the machine has a credential; it
		// resolves to the worker role, so this has to stay open.
		return true
	case "/api/v1/jobs/poll":
		return r.Method == http.MethodGet
	case "/api/v1/capabilities":
		// An agent probes this before starting -egress-mode=server|decider, so
		// that a control plane too old for the endpoints is a startup error
		// rather than a silent deny-all.
		return r.Method == http.MethodGet
	case "/api/v1/policy/decide":
		return r.Method == http.MethodPost
	}

	// A worker's own heartbeat: /api/v1/poders/<its id>/heartbeat. The handler
	// additionally refuses a record that belongs to another workspace.
	if strings.HasPrefix(r.URL.Path, "/api/v1/poders/") &&
		strings.HasSuffix(r.URL.Path, "/heartbeat") && r.Method == http.MethodPost {
		return true
	}

	// GET /api/v1/sandboxes/<name>/egress — an agent in -egress-mode=server polls
	// the policy of the sandbox it runs. Ownership and tenant are enforced by the
	// handler.
	if r.Method == http.MethodGet &&
		strings.HasPrefix(r.URL.Path, "/api/v1/sandboxes/") &&
		strings.HasSuffix(r.URL.Path, "/egress") {
		return true
	}

	return false
}
