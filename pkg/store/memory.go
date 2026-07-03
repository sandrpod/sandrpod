package store

import (
	"fmt"
	"sync"
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

// MemTokenRepo is an in-memory APITokenRepository, keyed by hash. Ephemeral:
// issued tokens are lost on restart (use the SQLite backend to persist).
type MemTokenRepo struct {
	mu sync.Mutex
	m  map[string]*sandpod.APIToken // hash -> token
}

// NewMemTokenRepo returns an empty in-memory token repository.
func NewMemTokenRepo() *MemTokenRepo { return &MemTokenRepo{m: map[string]*sandpod.APIToken{}} }

func (r *MemTokenRepo) Create(t *sandpod.APIToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[t.Hash]; ok {
		return fmt.Errorf("store: api token already exists")
	}
	cp := *t
	r.m[t.Hash] = &cp
	return nil
}

func (r *MemTokenRepo) List() ([]*sandpod.APIToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*sandpod.APIToken, 0, len(r.m))
	for _, t := range r.m {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

func (r *MemTokenRepo) DeleteByPrefix(prefix string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []string
	for h, t := range r.m {
		if t.Prefix == prefix {
			delete(r.m, h)
			removed = append(removed, h)
		}
	}
	return removed, nil
}

// NewMemoryStores constructs the default in-memory Stores.
// Used when no -db flag is provided; data is lost on server restart.
func NewMemoryStores() Stores {
	return Stores{
		Sandboxes: &MemSandboxRepo{sandpod.NewSandboxStore()},
		Poders:    &MemPoderRepo{sandpod.NewPoderStore()},
		Jobs:      &MemJobRepo{sandpod.NewJobStore()},
		Tokens:    NewMemTokenRepo(),
	}
}
