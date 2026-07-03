package store

import (
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/sandpod"
)

// Compile-time interface satisfaction checks.
var (
	_ sandpod.SandboxRepository = &MemSandboxRepo{}
	_ sandpod.PoderRepository   = &MemPoderRepo{}
	_ sandpod.JobRepository     = &MemJobRepo{}
)

// newTestStores is a helper that returns a freshly constructed Stores and fails
// the test if any field is nil.
func newTestStores(t *testing.T) Stores {
	t.Helper()
	s := NewMemoryStores()
	if s.Sandboxes == nil {
		t.Fatal("NewMemoryStores: Sandboxes is nil")
	}
	if s.Poders == nil {
		t.Fatal("NewMemoryStores: Poders is nil")
	}
	if s.Jobs == nil {
		t.Fatal("NewMemoryStores: Jobs is nil")
	}
	return s
}

// newTestJob builds a minimal PENDING job for use in tests.
func newTestJob(t *testing.T, id string) *sandpod.Job {
	t.Helper()
	return &sandpod.Job{
		ID:        id,
		Type:      sandpod.JobTypeCreateSandbox,
		Status:    sandpod.JobStatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// ---------- NewMemoryStores ----------

func TestNewMemoryStores_NonNilFields(t *testing.T) {
	newTestStores(t) // helper asserts non-nil; reaching here means pass
}

// ---------- MemSandboxRepo round-trip ----------

func TestMemSandboxRepo_AddGetRoundTrip(t *testing.T) {
	s := newTestStores(t)

	sb := &sandpod.SandboxInfo{
		ID:        "sb-001",
		Name:      "test-sandbox",
		Region:    "local",
		State:     sandpod.StatePending,
		CreatedAt: time.Now(),
	}

	if err := s.Sandboxes.Add(sb); err != nil {
		t.Fatalf("Add: unexpected error: %v", err)
	}

	got, ok := s.Sandboxes.Get("test-sandbox")
	if !ok {
		t.Fatal("Get: expected sandbox to exist, got false")
	}
	if got.ID != sb.ID {
		t.Errorf("Get: ID mismatch: want %q, got %q", sb.ID, got.ID)
	}
	if got.Name != sb.Name {
		t.Errorf("Get: Name mismatch: want %q, got %q", sb.Name, got.Name)
	}
}

func TestMemSandboxRepo_AddDuplicate_ReturnsError(t *testing.T) {
	s := newTestStores(t)

	sb := &sandpod.SandboxInfo{ID: "sb-dup", Name: "dup-sandbox"}
	if err := s.Sandboxes.Add(sb); err != nil {
		t.Fatalf("first Add: unexpected error: %v", err)
	}
	if err := s.Sandboxes.Add(sb); err == nil {
		t.Fatal("second Add: expected error for duplicate, got nil")
	}
}

func TestMemSandboxRepo_GetMissing_ReturnsFalse(t *testing.T) {
	s := newTestStores(t)
	_, ok := s.Sandboxes.Get("nonexistent")
	if ok {
		t.Fatal("Get: expected false for missing sandbox, got true")
	}
}

func TestMemSandboxRepo_List(t *testing.T) {
	s := newTestStores(t)

	names := []string{"alpha", "beta", "gamma"}
	for _, name := range names {
		if err := s.Sandboxes.Add(&sandpod.SandboxInfo{ID: name, Name: name}); err != nil {
			t.Fatalf("Add %q: %v", name, err)
		}
	}

	list := s.Sandboxes.List()
	if len(list) != len(names) {
		t.Errorf("List: want %d items, got %d", len(names), len(list))
	}
}

func TestMemSandboxRepo_Delete(t *testing.T) {
	s := newTestStores(t)

	sb := &sandpod.SandboxInfo{ID: "del-id", Name: "del-sandbox"}
	if err := s.Sandboxes.Add(sb); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Sandboxes.Delete("del-sandbox"); err != nil {
		t.Fatalf("Delete: unexpected error: %v", err)
	}
	if _, ok := s.Sandboxes.Get("del-sandbox"); ok {
		t.Fatal("Get after Delete: expected false, got true")
	}
}

func TestMemSandboxRepo_DeleteMissing_ReturnsError(t *testing.T) {
	s := newTestStores(t)
	if err := s.Sandboxes.Delete("ghost"); err == nil {
		t.Fatal("Delete non-existent: expected error, got nil")
	}
}

func TestMemSandboxRepo_Update(t *testing.T) {
	s := newTestStores(t)

	sb := &sandpod.SandboxInfo{ID: "upd-id", Name: "upd-sandbox", State: sandpod.StatePending}
	if err := s.Sandboxes.Add(sb); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := s.Sandboxes.Update("upd-sandbox", func(info *sandpod.SandboxInfo) {
		info.State = sandpod.StateRunning
	}); err != nil {
		t.Fatalf("Update: unexpected error: %v", err)
	}

	got, _ := s.Sandboxes.Get("upd-sandbox")
	if got.State != sandpod.StateRunning {
		t.Errorf("Update: state mismatch: want %q, got %q", sandpod.StateRunning, got.State)
	}
}

func TestMemSandboxRepo_ListByPoderID(t *testing.T) {
	s := newTestStores(t)

	_ = s.Sandboxes.Add(&sandpod.SandboxInfo{ID: "1", Name: "sb1", PoderID: "poder-A"})
	_ = s.Sandboxes.Add(&sandpod.SandboxInfo{ID: "2", Name: "sb2", PoderID: "poder-A"})
	_ = s.Sandboxes.Add(&sandpod.SandboxInfo{ID: "3", Name: "sb3", PoderID: "poder-B"})

	results := s.Sandboxes.ListByPoderID("poder-A")
	if len(results) != 2 {
		t.Errorf("ListByPoderID: want 2, got %d", len(results))
	}
}

// ---------- MemJobRepo: PollJobs ----------

func TestMemJobRepo_PollJobs_ReturnsPendingAsInProgress(t *testing.T) {
	s := newTestStores(t)

	j1 := newTestJob(t, "job-1")
	j2 := newTestJob(t, "job-2")
	for _, j := range []*sandpod.Job{j1, j2} {
		if err := s.Jobs.AddJob(j); err != nil {
			t.Fatalf("AddJob %q: %v", j.ID, err)
		}
	}

	polled, err := s.Jobs.PollJobs(1*time.Second, 10)
	if err != nil {
		t.Fatalf("PollJobs: unexpected error: %v", err)
	}
	if len(polled) != 2 {
		t.Fatalf("PollJobs: want 2 jobs, got %d", len(polled))
	}
	for _, j := range polled {
		if j.Status != sandpod.JobStatusInProgress {
			t.Errorf("PollJobs: job %q status = %q, want IN_PROGRESS", j.ID, j.Status)
		}
	}
}

func TestMemJobRepo_PollJobs_PersistsInProgressState(t *testing.T) {
	s := newTestStores(t)

	j := newTestJob(t, "job-persist")
	if err := s.Jobs.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	if _, err := s.Jobs.PollJobs(1*time.Second, 10); err != nil {
		t.Fatalf("PollJobs: %v", err)
	}

	// Verify the stored copy was updated to IN_PROGRESS.
	all := s.Jobs.ListJobs()
	if len(all) != 1 {
		t.Fatalf("ListJobs: want 1, got %d", len(all))
	}
	if all[0].Status != sandpod.JobStatusInProgress {
		t.Errorf("ListJobs after poll: status = %q, want IN_PROGRESS", all[0].Status)
	}
}

func TestMemJobRepo_PollJobs_RespectsLimit(t *testing.T) {
	s := newTestStores(t)

	for range 5 {
		id := sandpod.GenerateJobID()
		if err := s.Jobs.AddJob(newTestJob(t, id)); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}

	polled, err := s.Jobs.PollJobs(1*time.Second, 3)
	if err != nil {
		t.Fatalf("PollJobs: %v", err)
	}
	if len(polled) != 3 {
		t.Errorf("PollJobs limit=3: want 3, got %d", len(polled))
	}
}

func TestMemJobRepo_PollJobs_EmptyQueue_ReturnsEmpty(t *testing.T) {
	s := newTestStores(t)

	polled, err := s.Jobs.PollJobs(1*time.Second, 10)
	if err != nil {
		t.Fatalf("PollJobs: %v", err)
	}
	if len(polled) != 0 {
		t.Errorf("PollJobs on empty queue: want 0, got %d", len(polled))
	}
}

// ---------- MemJobRepo: AddJob / GetJob / UpdateJob ----------

func TestMemJobRepo_AddJobDuplicate_ReturnsError(t *testing.T) {
	s := newTestStores(t)

	j := newTestJob(t, "dup-job")
	if err := s.Jobs.AddJob(j); err != nil {
		t.Fatalf("first AddJob: %v", err)
	}
	if err := s.Jobs.AddJob(j); err == nil {
		t.Fatal("second AddJob: expected error for duplicate, got nil")
	}
}

func TestMemJobRepo_GetJob(t *testing.T) {
	s := newTestStores(t)

	j := newTestJob(t, "get-job")
	if err := s.Jobs.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	got, ok := s.Jobs.GetJob("get-job")
	if !ok {
		t.Fatal("GetJob: expected true, got false")
	}
	if got.ID != "get-job" {
		t.Errorf("GetJob: ID = %q, want %q", got.ID, "get-job")
	}
}

func TestMemJobRepo_GetJobMissing_ReturnsFalse(t *testing.T) {
	s := newTestStores(t)
	_, ok := s.Jobs.GetJob("ghost-job")
	if ok {
		t.Fatal("GetJob non-existent: expected false, got true")
	}
}

func TestMemJobRepo_UpdateJob(t *testing.T) {
	s := newTestStores(t)

	j := newTestJob(t, "upd-job")
	if err := s.Jobs.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	if err := s.Jobs.UpdateJob("upd-job", func(job *sandpod.Job) {
		job.Status = sandpod.JobStatusCompleted
	}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}

	got, _ := s.Jobs.GetJob("upd-job")
	if got.Status != sandpod.JobStatusCompleted {
		t.Errorf("UpdateJob: status = %q, want COMPLETED", got.Status)
	}
}

func TestMemTokenRepo(t *testing.T) {
	r := NewMemTokenRepo()
	tok := &sandpod.APIToken{Name: "alice", Prefix: "e2b_aaaabbbb", Hash: "hash-1", Role: "user", CreatedAt: time.Now()}
	if err := r.Create(tok); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Create(tok); err == nil {
		t.Error("duplicate hash should be rejected")
	}
	list, _ := r.List()
	if len(list) != 1 || list[0].Name != "alice" || list[0].Hash != "hash-1" {
		t.Fatalf("list: %+v", list)
	}
	// Non-matching prefix removes nothing.
	if removed, _ := r.DeleteByPrefix("e2b_nope"); len(removed) != 0 {
		t.Errorf("delete miss: want none, got %v", removed)
	}
	removed, _ := r.DeleteByPrefix("e2b_aaaabbbb")
	if len(removed) != 1 || removed[0] != "hash-1" {
		t.Errorf("delete: want [hash-1], got %v", removed)
	}
	if list, _ := r.List(); len(list) != 0 {
		t.Errorf("after delete: want empty, got %+v", list)
	}
}
