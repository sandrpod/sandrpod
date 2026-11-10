// Copyright 2026 SandrPod Contributors
// GPU guardrails: a GPU sandbox costs ten to thirty times what a CPU sandbox
// does, so an agent that leaves one running overnight is an unbudgeted bill. The
// platform enforces two limits per owner — how many may run at once, and how long
// one may live — both configurable, both defaulting to something a private
// install can afford.

package server

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

const (
	// defaultGPUPerOwner is deliberately 1: a second GPU sandbox is almost never
	// two independent experiments, it is one job that forgot to clean up.
	defaultGPUPerOwner = 1
	// defaultGPUMaxTTL bounds a GPU sandbox's idle TTL. It stops the "start it
	// and walk away" pattern without preventing a real training run, which
	// should be extending its TTL with activity (LastActivity refreshes on use).
	defaultGPUMaxTTL = 2 * time.Hour
)

// gpuQuota is the resolved policy for one create.
type gpuQuota struct {
	MaxPerOwner int
	MaxTTL      time.Duration
}

// gpuQuotaFromEnv reads SANDRPOD_GPU_MAX_PER_OWNER and SANDRPOD_GPU_MAX_TTL.
// Zero or negative disables that half of the guardrail (an operator running a
// trusted internal deployment may want that); unset takes the defaults.
func gpuQuotaFromEnv() gpuQuota {
	q := gpuQuota{MaxPerOwner: defaultGPUPerOwner, MaxTTL: defaultGPUMaxTTL}
	if v := strings.TrimSpace(os.Getenv("SANDRPOD_GPU_MAX_PER_OWNER")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.MaxPerOwner = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("SANDRPOD_GPU_MAX_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			q.MaxTTL = d
		}
	}
	return q
}

// checkGPUQuota answers "" when the create may proceed, or the refusal a caller
// should surface. It counts live GPU sandboxes owned by the same identity —
// terminated and errored records do not hold a slot, because the user cannot
// clean up a record the platform already gave up on.
func checkGPUQuota(q gpuQuota, req *podpkg.CreateSandboxRequest, owner string, existing []*podpkg.SandboxInfo) string {
	if req.GPU <= 0 {
		return ""
	}
	if q.MaxPerOwner > 0 {
		live := 0
		for _, sb := range existing {
			if sb.Owner != owner || sb.GPU <= 0 {
				continue
			}
			if sb.State == podpkg.StateTerminated || sb.State == podpkg.StateError {
				continue
			}
			live++
		}
		if live >= q.MaxPerOwner {
			return fmt.Sprintf(
				"GPU sandboxes are capped at %d per user; you already have %d running. "+
					"Stop or delete one first (its /workspace data survives a stop).",
				q.MaxPerOwner, live)
		}
	}
	return ""
}

// clampGPUTTL bounds a GPU sandbox's idle TTL. It clamps rather than refuses:
// an agent that asks for a day gets two hours and a warning in the log, instead
// of a create that fails on a setting the agent did not choose deliberately.
func clampGPUTTL(q gpuQuota, req *podpkg.CreateSandboxRequest) (clamped bool, from, to time.Duration) {
	if req.GPU <= 0 || q.MaxTTL <= 0 || req.TTLSeconds <= 0 {
		return false, 0, 0
	}
	asked := time.Duration(req.TTLSeconds) * time.Second
	if asked <= q.MaxTTL {
		return false, 0, 0
	}
	req.TTLSeconds = int64(q.MaxTTL / time.Second)
	return true, asked, q.MaxTTL
}
