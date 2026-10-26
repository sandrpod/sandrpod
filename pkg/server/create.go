// Copyright 2026 SandrPod Contributors
// Shared sandbox-create flow, used by both the synchronous POST /sandboxes
// path and the async (job-based) path.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/sandrpod/sandrpod/pkg/egress"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// Admission gates sandbox creation: quota, metering, risk checks — whatever a
// deployment wants to refuse before a container exists.
//
// It is called twice on purpose. The handlers call it early so a refusal is a
// clean 4xx with the caller's request still in front of them; runSandboxCreate
// calls it again at the choke point so a future entry point cannot skip it.
// Implementations must therefore be idempotent: a second call for the same
// request must reach the same verdict.
type Admission interface {
	Admit(ctx context.Context, id Identity, req *podpkg.CreateSandboxRequest) error
}

// ErrQuotaExceeded is the refusal an Admission returns when the caller is at
// their limit. Callers map it to their own surface (429 for the native API,
// e2bcompat.ErrQuotaExceeded for the E2B gateway).
var ErrQuotaExceeded = errors.New("sandbox quota reached")

// admissionFor returns the deployment's gate, or the built-in quota rule. Both
// New and the E2B gateway need the same one, and it must be the same object:
// a deployment that counts usage would otherwise count twice.
func admissionFor(cfg Config, store podpkg.SandboxRepository) Admission {
	if cfg.Admission != nil {
		return cfg.Admission
	}
	return quotaAdmission{store: store, max: cfg.MaxSandboxesPerOwner}
}

// quotaAdmission is the built-in rule: MaxSandboxesPerOwner, admins exempt.
type quotaAdmission struct {
	store podpkg.SandboxRepository
	max   int
}

func (q quotaAdmission) Admit(_ context.Context, id Identity, _ *podpkg.CreateSandboxRequest) error {
	if id.IsAdmin() || q.max <= 0 {
		return nil
	}
	if quotaExceeded(q.store, id.Name, q.max) {
		return ErrQuotaExceeded
	}
	return nil
}

// runSandboxCreate executes the full create flow: schedule (reuse or provision
// a poder), record the sandbox, create the container through the tunnel, and
// finalize job + sandbox state.
//
// When preJobID is non-empty (async path), that job record already exists and
// is updated as the flow progresses — the scheduler's own job object is only
// used for its poder placement. When empty (sync path), the scheduler's job is
// stored and its ID returned.
func runSandboxCreate(
	sched *podpkg.Scheduler,
	sandboxStore podpkg.SandboxRepository,
	poderStore podpkg.PoderRepository,
	jobStore podpkg.JobRepository,
	tunnelStore *tunnel.TunnelStore,
	req *podpkg.CreateSandboxRequest,
	preJobID string,
	owner string,
	admission Admission,
	requester Identity,
) (*podpkg.SandboxInfo, string, error) {
	failBoth := func(jobID, msg string) {
		notifyEvent("sandbox.error", map[string]any{
			"name": req.Name, "provider": req.ProviderType, "error": msg,
			"owner": owner, "cpu_cores": req.CPUCores, "memory_mb": req.MemoryMB,
		})
		if jobID != "" {
			_ = jobStore.UpdateJob(jobID, func(j *podpkg.Job) {
				j.Status = podpkg.JobStatusFailed
				j.ErrorMessage = msg
			})
		}
		_ = sandboxStore.Update(req.Name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateError })
	}

	// Placement: the caller's tenant decides which workers are eligible. It
	// never reaches the wire — the poder does not need to know.
	if requester.Tenant != "" {
		req.Workspace = requester.Tenant
	}

	// The choke point: every path that provisions a sandbox passes through
	// here, so a new entry point cannot silently skip the deployment's gate.
	// Handlers already refused early for a better error surface; Admission is
	// required to be idempotent for exactly this reason.
	//
	// A refusal here must also finish the job and the record. The async path
	// has already written a PENDING sandbox by now, and a caller that polls it
	// would wait forever for a state that is never coming.
	if admission != nil {
		if err := admission.Admit(context.Background(), requester, req); err != nil {
			failBoth(preJobID, fmt.Sprintf("failed to create sandbox: %v", err))
			// Wrap, not flatten: the caller's error mapping (409 for a missing
			// capability, e2bcompat.ErrQuotaExceeded for the E2B surface) reads
			// the chain.
			return nil, preJobID, fmt.Errorf("failed to create sandbox: %w", err)
		}
	}

	// 1. Schedule: reuse an available poder or provision a VM (minutes).
	// Detached from any request context so client disconnects can't abort it.
	schedCtx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	schedJob, err := sched.ScheduleSandboxCreation(schedCtx, req)
	cancel()
	if err != nil {
		log.Printf("create sandbox %s (provider=%s) failed: %v", req.Name, req.ProviderType, err)
		failBoth(preJobID, err.Error())
		return nil, preJobID, fmt.Errorf("failed to create sandbox: %w", err)
	}

	// 2. Job bookkeeping.
	jobID := preJobID
	if jobID == "" {
		jobID = schedJob.ID
		schedJob.Owner = owner
		if err := jobStore.AddJob(schedJob); err != nil {
			return nil, jobID, err
		}
	} else {
		_ = jobStore.UpdateJob(jobID, func(j *podpkg.Job) {
			j.Status = podpkg.JobStatusInProgress
			j.PoderID = schedJob.PoderID
			j.PoderURL = schedJob.PoderURL
		})
	}

	// 3. Upsert the sandbox record in PENDING with poder placement filled in.
	sbArch, sbOS, sbOSVersion := "", "", ""
	if pi, ok := poderStore.Get(schedJob.PoderID); ok {
		sbArch = pi.Resources.Arch
		sbOS = pi.Resources.OS
		sbOSVersion = pi.Resources.OSVersion
	}
	if _, exists := sandboxStore.Get(req.Name); exists {
		_ = sandboxStore.Update(req.Name, func(s *podpkg.SandboxInfo) {
			s.PoderID = schedJob.PoderID
			s.ProxyURL = "tunnel://" + schedJob.PoderID
			s.Arch = sbArch
			s.OS = sbOS
			s.OSVersion = sbOSVersion
			s.TTLSeconds = req.TTLSeconds
			s.CPUCores = req.CPUCores
			s.MemoryMB = req.MemoryMB
			// A create request may carry a new policy (same sandbox name,
			// recreated); keep the record in step with the request.
			if req.Network != nil {
				s.Network = req.Network
			}
			s.LastActivity = time.Now()
		})
	} else {
		_ = sandboxStore.Add(&podpkg.SandboxInfo{
			ID:           jobID,
			Name:         req.Name,
			Region:       req.Region,
			ProviderType: req.ProviderType,
			InstanceType: req.InstanceType,
			Owner:        owner,
			TTLSeconds:   req.TTLSeconds,
			CPUCores:     req.CPUCores,
			MemoryMB:     req.MemoryMB,
			Network:      req.Network,
			PoderID:      schedJob.PoderID,
			ProxyURL:     "tunnel://" + schedJob.PoderID,
			State:        podpkg.StatePending,
			Arch:         sbArch,
			OS:           sbOS,
			OSVersion:    sbOSVersion,
			CreatedAt:    time.Now(),
			LastActivity: time.Now(),
		})
	}

	// 4. Create the container on the poder through the tunnel. Detached and
	// bounded: by now the client has often disconnected.
	t, ok := tunnelStore.Get(schedJob.PoderID)
	if !ok {
		failBoth(jobID, "poder tunnel not available")
		return nil, jobID, fmt.Errorf("poder tunnel not available")
	}
	bodyBytes, _ := json.Marshal(req)
	poderCtx, poderCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer poderCancel()
	createReq, _ := http.NewRequestWithContext(poderCtx, http.MethodPost, "http://poder/sandboxes", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")

	resp, err := t.Client.Do(createReq)
	if err != nil {
		log.Printf("create sandbox %s: poder container create failed: %v", req.Name, err)
		failBoth(jobID, err.Error())
		return nil, jobID, fmt.Errorf("failed to create sandbox: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		log.Printf("create sandbox %s: poder error: %s", req.Name, string(respBody))
		failBoth(jobID, string(respBody))
		return nil, jobID, fmt.Errorf("poder error: %s", string(respBody))
	}

	// 5. Finalize.
	var poderResp map[string]any
	_ = json.Unmarshal(respBody, &poderResp)
	_ = jobStore.UpdateJob(jobID, func(j *podpkg.Job) {
		j.Status = podpkg.JobStatusCompleted
		if v, _ := poderResp["id"].(string); v != "" {
			j.SandboxID = v
			j.Result = &podpkg.JobResult{ProxyURL: "tunnel://" + schedJob.PoderID, SandboxID: v}
		}
		if v, _ := poderResp["ip"].(string); v != "" && j.Result != nil {
			j.Result.IP = v
		}
	})
	_ = sandboxStore.Update(req.Name, func(s *podpkg.SandboxInfo) {
		s.State = podpkg.StateRunning
		if v, _ := poderResp["id"].(string); v != "" {
			s.ID = v
		}
		if v, _ := poderResp["ip"].(string); v != "" {
			s.IP = v
		}
	})
	sb, _ := sandboxStore.Get(req.Name)
	if sb != nil {
		notifyEvent("sandbox.running", sandboxEvent(sb, map[string]any{"poder_id": schedJob.PoderID}))
	} else {
		notifyEvent("sandbox.running", map[string]any{"name": req.Name, "provider": req.ProviderType, "poder_id": schedJob.PoderID})
	}
	return sb, jobID, nil
}

// quotaExceeded reports whether owner already holds max sandboxes. max <= 0
// means unlimited. Both create paths — native POST /api/v1/sandboxes and the
// E2B gateway's POST /sandboxes — must consult this; a check written into only
// one of them is a cap that does nothing on the other surface.
func quotaExceeded(store podpkg.SandboxRepository, owner string, max int) bool {
	if max <= 0 {
		return false
	}
	owned := 0
	for _, sb := range store.List() {
		if sb.Owner == owner {
			owned++
		}
	}
	return owned >= max
}

// maxSandboxNameLen bounds the name so it stays usable as a map key, a log
// field, and a URL path segment.
const maxSandboxNameLen = 64

// sandboxNamePattern is what a name may contain. The container itself is named
// by a generated id, so this is not a shell- or Docker-safety boundary — it is
// about addressability: a name is a URL path segment, and one containing "/"
// (or empty) produces a sandbox that is created, runs, and can never be
// fetched or deleted again through the API. Nothing downstream rejects it, so
// this is the only place it can be caught.
var sandboxNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateSandboxName returns a reason the name is unusable, or "".
func validateSandboxName(name string) string {
	switch {
	case name == "":
		return "sandbox name is required"
	case len(name) > maxSandboxNameLen:
		return fmt.Sprintf("sandbox name is too long (%d > %d)", len(name), maxSandboxNameLen)
	case !sandboxNamePattern.MatchString(name):
		return "sandbox name must start with a letter or digit and contain only letters, digits, '.', '_' or '-'"
	}
	return ""
}

// decideForSandbox evaluates one destination against a sandbox's stored policy.
//
// No record, no policy, or a policy that no longer parses all deny: this is the
// per-connection gate, and the safe reading of "I cannot tell" is no. The reason
// says which of the three it was, because an operator reading the agent's log
// needs to know whether to create the sandbox, set a policy, or fix a typo.
func decideForSandbox(sb *podpkg.SandboxInfo, found bool, host string, port int) egress.Verdict {
	switch {
	case !found:
		return egress.Verdict{Code: egress.CodeNotAllowed, Host: host, Port: port,
			Reason: "this sandbox is not registered with the control plane; egress is denied"}
	case sb.Network == nil:
		return egress.Verdict{Code: egress.CodeNotAllowed, Host: host, Port: port,
			Reason: "the sandbox has no egress policy; add one with PUT /api/v1/sandboxes/{name}/egress"}
	}
	compiled, err := egress.Parse(*sb.Network)
	if err != nil {
		return egress.Verdict{Code: egress.CodeNotAllowed, Host: host, Port: port,
			Reason: "the sandbox's egress policy is invalid; egress is denied until it is fixed"}
	}
	return compiled.Decide(host, port)
}

// validateEgressPolicy compiles a policy without starting anything. It is the
// create-time and PUT-time gate: a malformed entry, an unknown wildcard shape,
// an unbracketed IPv6 literal with a port — all of it is reported here, while
// the caller is still on the line, instead of at the sandbox, where it would
// read as "the network is broken".
func validateEgressPolicy(p *egress.Policy) error {
	if p == nil {
		return nil
	}
	if _, err := egress.Parse(*p); err != nil {
		return err
	}
	return nil
}
