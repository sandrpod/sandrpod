package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// internalProvisionPath is where a peer asks this instance to deliver a create
// command through a tunnel it holds locally.
//
// A worker's tunnel is a live object in the process that accepted its WebSocket.
// Placement, however, is a database query: it can (and with enough workers, it
// regularly will) choose a worker connected to a different instance. Execute and
// the toolbox proxy handle that by forwarding the HTTP request, because replaying
// a stateless request against the owning node is safe. A create cannot be
// replayed — it admits, writes a record and enqueues a job — so only its *last
// step* is forwarded here: "you own this tunnel, please send this create down it".
const internalProvisionPath = "/internal/provision"

// internalProvisionHeader carries the platform token. Both instances share the
// same token store, so no additional secret is needed; a request without it is
// refused rather than trusted by network position.
const internalProvisionHeader = "X-Sandrpod-Internal"

type provisionRequest struct {
	Sandbox *podpkg.CreateSandboxRequest `json:"sandbox"`
	JobID   string                       `json:"job_id"`
	PoderID string                       `json:"poder_id"`
	Owner   string                       `json:"owner"`
}

// mountProvision registers the endpoint. It is a no-op for a single-instance
// deployment: without a node URL there are no peers to serve.
func mountProvision(mux *http.ServeMux, cfg Config, tunnelStore *tunnel.TunnelStore, sandboxStore podpkg.SandboxRepository, jobStore podpkg.JobRepository) {
	mux.HandleFunc(internalProvisionPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if cfg.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get(internalProvisionHeader)), []byte(cfg.Token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var req provisionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.Sandbox == nil || req.PoderID == "" {
			http.Error(w, "sandbox and poder_id are required", http.StatusBadRequest)
			return
		}
		t, ok := tunnelStore.Get(req.PoderID)
		if !ok {
			// The peer's view of ownership was stale (the worker reconnected
			// here, or left). Saying so lets the caller fail the record instead of
			// waiting on a tunnel that is not here either.
			http.Error(w, "tunnel not held by this instance", http.StatusConflict)
			return
		}

		sb, _, err := pushCreateToPoder(t, req.Sandbox, req.JobID, req.PoderID, sandboxStore, jobStore,
			func(msg string) { log.Printf("provision %s (from peer): %s", req.Sandbox.Name, msg) })
		if err != nil {
			log.Printf("provision %s (from peer): %v", req.Sandbox.Name, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "sandbox": sb, "poder_id": req.PoderID})
	})
}

// deliverCreateToPeer asks the node that holds poderID's tunnel to deliver the
// create command. It returns the sandbox the peer reports.
func deliverCreateToPeer(peerURL, token string, req *podpkg.CreateSandboxRequest, jobID, owner, poderID string) (*podpkg.SandboxInfo, error) {
	payload, err := json.Marshal(provisionRequest{Sandbox: req, JobID: jobID, PoderID: poderID, Owner: owner})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, peerURL+internalProvisionPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(internalProvisionHeader, token)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("peer %s: %w", peerURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s answered %d: %s", peerURL, resp.StatusCode, bytes.TrimSpace(body))
	}
	var out struct {
		Sandbox *podpkg.SandboxInfo `json:"sandbox"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("peer %s: bad response: %w", peerURL, err)
	}
	return out.Sandbox, nil
}

// wantsPeerDelivery reports whether this deployment can hand a create to a peer,
// and where: the node currently claiming the worker's tunnel.
func wantsPeerDelivery(cfg Config, owners podpkg.TunnelOwnerRepository, poderID string) (string, bool) {
	if cfg.NodeURL == "" || cfg.Token == "" || owners == nil {
		return "", false
	}
	owner, ok := owners.NodeFor(poderID)
	if !ok || owner == cfg.NodeURL {
		return "", false
	}
	return owner, true
}

var _ = errors.New
