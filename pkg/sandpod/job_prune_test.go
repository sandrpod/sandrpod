package sandpod

import (
	"testing"
	"time"
)

func TestJobStorePruneJobs(t *testing.T) {
	s := NewJobStore()
	old := time.Now().Add(-48 * time.Hour)
	for _, j := range []*Job{
		{ID: "old-done", Status: JobStatusCompleted, CreatedAt: old, UpdatedAt: old},
		{ID: "old-fail", Status: JobStatusFailed, CreatedAt: old, UpdatedAt: old},
		{ID: "old-pending", Status: JobStatusPending, CreatedAt: old, UpdatedAt: old},
		{ID: "fresh-done", Status: JobStatusCompleted, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	} {
		if err := s.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneJobs(time.Now().Add(-24 * time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("pruned=%d err=%v, want 2", n, err)
	}
	if _, ok := s.GetJob("old-pending"); !ok {
		t.Fatal("pending job was pruned")
	}
	if _, ok := s.GetJob("fresh-done"); !ok {
		t.Fatal("fresh job was pruned")
	}
}
