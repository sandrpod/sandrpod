// Copyright 2024 SandrPod
// Shared sandbox-create flow, used by both the synchronous POST /sandboxes
// path and the async (job-based) path.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

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
) (*podpkg.SandboxInfo, string, error) {
	failBoth := func(jobID, msg string) {
		if jobID != "" {
			_ = jobStore.UpdateJob(jobID, func(j *podpkg.Job) {
				j.Status = podpkg.JobStatusFailed
				j.ErrorMessage = msg
			})
		}
		_ = sandboxStore.Update(req.Name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateError })
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
	return sb, jobID, nil
}
