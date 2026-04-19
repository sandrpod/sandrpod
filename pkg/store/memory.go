package store

import (
	"time"

	"github.com/sandrpod/sandrpod/pkg/sandpod"
)

// MemSandboxRepo adapts *sandpod.SandboxStore to SandboxRepository.
// The embedded type already has the exact method set required by the interface.
type MemSandboxRepo struct{ *sandpod.SandboxStore }

// MemPoderRepo adapts *sandpod.PoderStore to PoderRepository.
type MemPoderRepo struct{ *sandpod.PoderStore }

// MemJobRepo adapts *sandpod.JobStore to JobRepository.
type MemJobRepo struct{ *sandpod.JobStore }

// PollJobs bridges the timeout argument: the in-memory JobStore carries an
// internal jobTimeout field set via SetJobTimeout, so we set it before calling.
func (r *MemJobRepo) PollJobs(jobTimeout time.Duration, limit int) ([]*sandpod.Job, error) {
	r.JobStore.SetJobTimeout(jobTimeout)
	return r.JobStore.PollJobs(0, limit)
}

// NewMemoryStores constructs the default in-memory Stores.
// Used when no -db flag is provided; data is lost on server restart.
func NewMemoryStores() Stores {
	return Stores{
		Sandboxes: &MemSandboxRepo{sandpod.NewSandboxStore()},
		Poders:    &MemPoderRepo{sandpod.NewPoderStore()},
		Jobs:      &MemJobRepo{sandpod.NewJobStore()},
	}
}
