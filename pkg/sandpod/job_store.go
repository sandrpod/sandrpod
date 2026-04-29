// Copyright 2024 SandrPod
// Job Store - in-memory job storage

package sandpod

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// JobStore is the in-memory job store.
type JobStore struct {
	mu          sync.RWMutex
	jobs        map[string]*Job
	queue       []*Job        // job queue ordered by creation time
	jobTimeout  time.Duration // job timeout duration, default 5 minutes
}

// NewJobStore creates a new JobStore.
func NewJobStore() *JobStore {
	return &JobStore{
		jobs:       make(map[string]*Job),
		queue:      make([]*Job, 0),
		jobTimeout: 5 * time.Minute, // default 5-minute timeout
	}
}

// SetJobTimeout sets the job timeout duration.
func (s *JobStore) SetJobTimeout(timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobTimeout = timeout
}

// AddJob adds a job to the store.
func (s *JobStore) AddJob(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.jobs[job.ID]; exists {
		return fmt.Errorf("job %s already exists", job.ID)
	}

	s.jobs[job.ID] = job
	s.queue = append(s.queue, job)
	s.sortQueue()

	return nil
}

// GetJob retrieves a job by ID.
func (s *JobStore) GetJob(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	job, ok := s.jobs[id]
	return job, ok
}

// UpdateJob applies an update function to a job.
func (s *JobStore) UpdateJob(id string, updateFn func(*Job)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}

	updateFn(job)
	job.UpdatedAt = time.Now()
	return nil
}

// PollJobs returns pending jobs (long-poll style).
func (s *JobStore) PollJobs(timeout time.Duration, limit int) ([]*Job, error) {
	s.mu.Lock()

	// First, reset any IN_PROGRESS jobs that have timed out back to PENDING.
	for _, job := range s.jobs {
		if job.Status == JobStatusInProgress {
			if time.Since(job.UpdatedAt) > s.jobTimeout {
				job.Status = JobStatusPending
				job.UpdatedAt = time.Now()
				log.Printf("Job %s timed out, resetting to PENDING", job.ID)
			}
		}
	}

	// Collect all PENDING jobs up to the requested limit.
	pending := make([]*Job, 0)
	for _, job := range s.queue {
		if job.Status == JobStatusPending {
			pending = append(pending, job)
			if len(pending) >= limit {
				break
			}
		}
	}

	// Mark the collected jobs as IN_PROGRESS.
	for _, job := range pending {
		job.Status = JobStatusInProgress
		job.UpdatedAt = time.Now()
	}

	s.mu.Unlock()

	return pending, nil
}

// ListJobs returns all jobs in the store.
func (s *JobStore) ListJobs() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	jobs := make([]*Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	return jobs
}

// sortQueue sorts the queue by creation time.
func (s *JobStore) sortQueue() {
	// Simple insertion sort.
	for i := 1; i < len(s.queue); i++ {
		for j := i; j > 0 && s.queue[j].CreatedAt.Before(s.queue[j-1].CreatedAt); j-- {
			s.queue[j], s.queue[j-1] = s.queue[j-1], s.queue[j]
		}
	}
}

// GenerateJobID generates a unique job ID.
func GenerateJobID() string {
	return fmt.Sprintf("job-%d-%s", time.Now().UnixNano(), randomString(8))
}

// randomString generates a random alphanumeric string of length n.
func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
		time.Sleep(time.Nanosecond)
	}
	return string(b)
}
