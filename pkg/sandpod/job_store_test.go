// Copyright 2024 SandrPod
// Unit tests for JobStore

package sandpod

import (
	"fmt"
	"testing"
	"time"
)

func newTestJob(id string, jobType JobType, status JobStatus) *Job {
	now := time.Now()
	return &Job{
		ID:           id,
		Type:         jobType,
		Status:       status,
		SandboxName:  fmt.Sprintf("sandbox-%s", id),
		Region:       "us-east-1",
		ProviderType: "local",
		PoderID:      "poder-1",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestNewJobStore(t *testing.T) {
	s := NewJobStore()
	if s == nil {
		t.Fatal("NewJobStore returned nil")
	}
	if s.jobTimeout != 5*time.Minute {
		t.Errorf("expected default 5m timeout, got %v", s.jobTimeout)
	}
	jobs := s.ListJobs()
	if len(jobs) != 0 {
		t.Errorf("expected empty store, got %d jobs", len(jobs))
	}
}

func TestJobStore_SetJobTimeout(t *testing.T) {
	s := NewJobStore()
	s.SetJobTimeout(2 * time.Minute)
	if s.jobTimeout != 2*time.Minute {
		t.Errorf("expected 2m timeout, got %v", s.jobTimeout)
	}
}

func TestJobStore_AddJob(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		s := NewJobStore()
		job := newTestJob("j1", JobTypeCreateSandbox, JobStatusPending)

		if err := s.AddJob(job); err != nil {
			t.Fatalf("AddJob failed: %v", err)
		}

		jobs := s.ListJobs()
		if len(jobs) != 1 {
			t.Errorf("expected 1 job, got %d", len(jobs))
		}
	})

	t.Run("duplicate returns error", func(t *testing.T) {
		s := NewJobStore()
		job := newTestJob("j1", JobTypeCreateSandbox, JobStatusPending)
		_ = s.AddJob(job)

		err := s.AddJob(job)
		if err == nil {
			t.Fatal("expected error for duplicate job, got nil")
		}
	})

	t.Run("multiple jobs added successfully", func(t *testing.T) {
		s := NewJobStore()
		for i := range 5 {
			job := newTestJob(fmt.Sprintf("j%d", i), JobTypeCreateSandbox, JobStatusPending)
			if err := s.AddJob(job); err != nil {
				t.Fatalf("AddJob(%d) failed: %v", i, err)
			}
		}
		if len(s.ListJobs()) != 5 {
			t.Errorf("expected 5 jobs, got %d", len(s.ListJobs()))
		}
	})
}

func TestJobStore_GetJob(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		s := NewJobStore()
		job := newTestJob("j1", JobTypeCreateSandbox, JobStatusPending)
		_ = s.AddJob(job)

		got, ok := s.GetJob("j1")
		if !ok {
			t.Fatal("expected to find j1, got not found")
		}
		if got.ID != "j1" {
			t.Errorf("expected ID j1, got %s", got.ID)
		}
	})

	t.Run("not found", func(t *testing.T) {
		s := NewJobStore()
		_, ok := s.GetJob("nonexistent")
		if ok {
			t.Fatal("expected not found for nonexistent job")
		}
	})
}

func TestJobStore_UpdateJob(t *testing.T) {
	t.Run("applies mutation and updates UpdatedAt", func(t *testing.T) {
		s := NewJobStore()
		job := newTestJob("j1", JobTypeCreateSandbox, JobStatusPending)
		originalUpdatedAt := job.UpdatedAt
		_ = s.AddJob(job)

		time.Sleep(time.Millisecond)

		err := s.UpdateJob("j1", func(j *Job) {
			j.Status = JobStatusCompleted
			j.Result = &JobResult{SandboxID: "sandbox-abc", ExitCode: 0}
		})
		if err != nil {
			t.Fatalf("UpdateJob failed: %v", err)
		}

		got, _ := s.GetJob("j1")
		if got.Status != JobStatusCompleted {
			t.Errorf("expected COMPLETED status, got %s", got.Status)
		}
		if got.Result == nil || got.Result.SandboxID != "sandbox-abc" {
			t.Errorf("expected result with sandbox-abc, got %+v", got.Result)
		}
		if !got.UpdatedAt.After(originalUpdatedAt) {
			t.Errorf("UpdatedAt should be after original: original=%v, got=%v", originalUpdatedAt, got.UpdatedAt)
		}
	})

	t.Run("missing returns error", func(t *testing.T) {
		s := NewJobStore()
		err := s.UpdateJob("nonexistent", func(j *Job) {
			j.Status = JobStatusCompleted
		})
		if err == nil {
			t.Fatal("expected error for missing job, got nil")
		}
	})
}

func TestJobStore_PollJobs(t *testing.T) {
	t.Run("returns pending jobs up to limit and marks them IN_PROGRESS", func(t *testing.T) {
		s := NewJobStore()
		for i := range 5 {
			job := newTestJob(fmt.Sprintf("j%d", i), JobTypeCreateSandbox, JobStatusPending)
			_ = s.AddJob(job)
		}

		polled, err := s.PollJobs(5*time.Minute, 3)
		if err != nil {
			t.Fatalf("PollJobs failed: %v", err)
		}
		if len(polled) != 3 {
			t.Errorf("expected 3 jobs, got %d", len(polled))
		}
		for _, j := range polled {
			if j.Status != JobStatusInProgress {
				t.Errorf("expected IN_PROGRESS, got %s for job %s", j.Status, j.ID)
			}
		}
	})

	t.Run("second call returns remaining pending jobs", func(t *testing.T) {
		s := NewJobStore()
		for i := range 5 {
			job := newTestJob(fmt.Sprintf("j%d", i), JobTypeCreateSandbox, JobStatusPending)
			_ = s.AddJob(job)
		}

		first, _ := s.PollJobs(5*time.Minute, 3)
		if len(first) != 3 {
			t.Fatalf("expected 3 on first poll, got %d", len(first))
		}

		second, _ := s.PollJobs(5*time.Minute, 3)
		if len(second) != 2 {
			t.Errorf("expected 2 remaining jobs on second poll, got %d", len(second))
		}
	})

	t.Run("third call returns empty when no pending left", func(t *testing.T) {
		s := NewJobStore()
		for i := range 4 {
			job := newTestJob(fmt.Sprintf("j%d", i), JobTypeCreateSandbox, JobStatusPending)
			_ = s.AddJob(job)
		}

		s.PollJobs(5*time.Minute, 4)
		third, _ := s.PollJobs(5*time.Minute, 4)
		if len(third) != 0 {
			t.Errorf("expected 0 jobs on third poll, got %d", len(third))
		}
	})

	t.Run("stale IN_PROGRESS jobs are reset to PENDING and reclaimed", func(t *testing.T) {
		s := NewJobStore()
		s.SetJobTimeout(10 * time.Millisecond) // very short timeout

		job := newTestJob("j-stale", JobTypeCreateSandbox, JobStatusPending)
		_ = s.AddJob(job)

		// First poll: marks j-stale as IN_PROGRESS
		first, _ := s.PollJobs(10*time.Millisecond, 1)
		if len(first) != 1 {
			t.Fatalf("expected 1 job on first poll, got %d", len(first))
		}

		// Wait for the job to become stale
		time.Sleep(20 * time.Millisecond)

		// Second poll: stale job should be reset and re-claimed
		second, _ := s.PollJobs(10*time.Millisecond, 1)
		if len(second) != 1 {
			t.Errorf("expected stale job to be reclaimed, got %d jobs", len(second))
		}
		if second[0].ID != "j-stale" {
			t.Errorf("expected j-stale to be reclaimed, got %s", second[0].ID)
		}
	})

	t.Run("ordering: returns jobs sorted by CreatedAt ASC", func(t *testing.T) {
		s := NewJobStore()

		// Create jobs with distinct timestamps
		for i := 2; i >= 0; i-- {
			job := &Job{
				ID:          fmt.Sprintf("j%d", i),
				Type:        JobTypeCreateSandbox,
				Status:      JobStatusPending,
				SandboxName: fmt.Sprintf("sandbox-j%d", i),
				CreatedAt:   time.Now().Add(time.Duration(i) * time.Millisecond),
				UpdatedAt:   time.Now(),
			}
			_ = s.AddJob(job)
			time.Sleep(time.Millisecond)
		}

		polled, _ := s.PollJobs(5*time.Minute, 10)
		if len(polled) != 3 {
			t.Fatalf("expected 3 jobs, got %d", len(polled))
		}
		// Each polled job should have a CreatedAt <= next job's CreatedAt
		for i := 1; i < len(polled); i++ {
			if polled[i].CreatedAt.Before(polled[i-1].CreatedAt) {
				t.Errorf("jobs not sorted by CreatedAt ASC: job[%d]=%v > job[%d]=%v",
					i-1, polled[i-1].CreatedAt, i, polled[i].CreatedAt)
			}
		}
	})

	t.Run("empty store returns empty slice", func(t *testing.T) {
		s := NewJobStore()
		polled, err := s.PollJobs(5*time.Minute, 10)
		if err != nil {
			t.Fatalf("PollJobs on empty store failed: %v", err)
		}
		if len(polled) != 0 {
			t.Errorf("expected 0 jobs, got %d", len(polled))
		}
	})

	t.Run("does not return completed jobs", func(t *testing.T) {
		s := NewJobStore()
		pending := newTestJob("j-pending", JobTypeCreateSandbox, JobStatusPending)
		completed := newTestJob("j-completed", JobTypeCreateSandbox, JobStatusCompleted)
		failed := newTestJob("j-failed", JobTypeCreateSandbox, JobStatusFailed)
		_ = s.AddJob(pending)
		_ = s.AddJob(completed)
		_ = s.AddJob(failed)

		polled, _ := s.PollJobs(5*time.Minute, 10)
		if len(polled) != 1 {
			t.Errorf("expected only 1 pending job, got %d", len(polled))
		}
		if polled[0].ID != "j-pending" {
			t.Errorf("expected j-pending, got %s", polled[0].ID)
		}
	})
}

func TestJobStore_ListJobs(t *testing.T) {
	t.Run("returns all jobs", func(t *testing.T) {
		s := NewJobStore()
		_ = s.AddJob(newTestJob("j1", JobTypeCreateSandbox, JobStatusPending))
		_ = s.AddJob(newTestJob("j2", JobTypeDeleteSandbox, JobStatusCompleted))
		_ = s.AddJob(newTestJob("j3", JobTypeStopSandbox, JobStatusFailed))

		jobs := s.ListJobs()
		if len(jobs) != 3 {
			t.Errorf("expected 3 jobs, got %d", len(jobs))
		}
	})

	t.Run("empty store returns empty slice", func(t *testing.T) {
		s := NewJobStore()
		jobs := s.ListJobs()
		if len(jobs) != 0 {
			t.Errorf("expected 0 jobs, got %d", len(jobs))
		}
	})
}

func TestGenerateJobID(t *testing.T) {
	id1 := GenerateJobID()
	id2 := GenerateJobID()

	if id1 == "" {
		t.Error("GenerateJobID returned empty string")
	}
	if id1 == id2 {
		t.Errorf("GenerateJobID returned duplicate IDs: %s", id1)
	}
}

// TestJobStore_SortQueue_SwapTriggered uses fixed timestamps to guarantee
// that sortQueue's inner swap branch is exercised.
func TestJobStore_SortQueue_SwapTriggered(t *testing.T) {
	s := NewJobStore()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Insert in reverse CreatedAt order: j3 (latest) first, j1 (earliest) last.
	// Each AddJob calls sortQueue — the swap branch fires on j2 and j1 inserts.
	for _, item := range []struct {
		id     string
		offset time.Duration
	}{
		{"j3", 3 * time.Second},
		{"j2", 2 * time.Second},
		{"j1", 1 * time.Second},
	} {
		job := &Job{
			ID:        item.id,
			Type:      JobTypeCreateSandbox,
			Status:    JobStatusPending,
			CreatedAt: base.Add(item.offset),
			UpdatedAt: base,
		}
		if err := s.AddJob(job); err != nil {
			t.Fatalf("AddJob(%s): %v", item.id, err)
		}
	}

	polled, err := s.PollJobs(5*time.Minute, 10)
	if err != nil {
		t.Fatalf("PollJobs: %v", err)
	}
	if len(polled) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(polled))
	}
	want := []string{"j1", "j2", "j3"}
	for i, j := range polled {
		if j.ID != want[i] {
			t.Errorf("polled[%d].ID = %q, want %q", i, j.ID, want[i])
		}
	}
}
