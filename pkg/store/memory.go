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

func (r *MemTokenRepo) FindByHash(hash string) (*sandpod.APIToken, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.m[hash]; ok {
		cp := *t
		return &cp, true
	}
	return nil, false
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

// MemTunnelOwnerRepo is an in-memory TunnelOwnerRepository. Single-process, so
// the cross-instance forwarding it enables only matters with a shared SQL
// backend; here it just records local claims harmlessly.
type MemTunnelOwnerRepo struct {
	mu sync.Mutex
	m  map[string]string // key -> node url
}

// NewMemTunnelOwnerRepo returns an empty in-memory tunnel-owner repository.
func NewMemTunnelOwnerRepo() *MemTunnelOwnerRepo { return &MemTunnelOwnerRepo{m: map[string]string{}} }

func (r *MemTunnelOwnerRepo) Claim(key, nodeURL string) error {
	r.mu.Lock()
	r.m[key] = nodeURL
	r.mu.Unlock()
	return nil
}

func (r *MemTunnelOwnerRepo) Release(key, nodeURL string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[key] == nodeURL {
		delete(r.m, key)
	}
	return nil
}

func (r *MemTunnelOwnerRepo) NodeFor(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.m[key]
	return n, ok
}

// NewMemoryStores constructs the default in-memory Stores.
// Used when no -db flag is provided; data is lost on server restart.
func NewMemoryStores() Stores {
	return Stores{
		Sandboxes:    &MemSandboxRepo{sandpod.NewSandboxStore()},
		Poders:       &MemPoderRepo{sandpod.NewPoderStore()},
		Jobs:         &MemJobRepo{sandpod.NewJobStore()},
		Tokens:       NewMemTokenRepo(),
		TunnelOwners: NewMemTunnelOwnerRepo(),
	}
}
