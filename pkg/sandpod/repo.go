package sandpod

import "time"

// SandboxRepository is the read/write contract for sandbox records.
// All implementations must be safe for concurrent use.
type SandboxRepository interface {
	Add(sb *SandboxInfo) error
	Get(name string) (*SandboxInfo, bool)
	Update(name string, fn func(*SandboxInfo)) error
	List() []*SandboxInfo
	ListByPoderID(poderID string) []*SandboxInfo
	Delete(name string) error
}

// PoderRepository is the read/write contract for Poder (worker node) records.
type PoderRepository interface {
	Register(req *RegisterPoderRequest) (*PoderInfo, error)
	Heartbeat(id string, usage *HeartbeatRequest) error
	Get(id string) (*PoderInfo, bool)
	List() []*PoderInfo
	// SelectBest returns the least-loaded available Poder matching region and
	// providerType filters (empty string matches any).
	SelectBest(region, providerType string) (*PoderInfo, error)
	UpdateUsage(id string, fn func(*PoderUsage)) error
	SetOffline(id string)
	Delete(id string) error
}

// JobRepository is the read/write contract for async job records.
type JobRepository interface {
	AddJob(job *Job) error
	GetJob(id string) (*Job, bool)
	UpdateJob(id string, fn func(*Job)) error
	// PollJobs atomically resets timed-out IN_PROGRESS jobs to PENDING, then
	// claims up to limit PENDING jobs (marks them IN_PROGRESS) and returns them.
	PollJobs(jobTimeout time.Duration, limit int) ([]*Job, error)
	ListJobs() []*Job
}

// Stores groups the repositories for dependency injection into handlers and the
// scheduler.
type Stores struct {
	Sandboxes SandboxRepository
	Poders    PoderRepository
	Jobs      JobRepository
	// Tokens persists issued API tokens (nil for backends that predate it; the
	// server treats nil as "no DB-backed tokens").
	Tokens APITokenRepository
}
