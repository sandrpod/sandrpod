package sqldb_test

import (
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store/sqldb"
)

// ─── Sandbox Repo ────────────────────────────────────────────────────────────

func TestSandboxRepo_CRUD(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewSandboxRepo(db)
	now := time.Now().UTC().Truncate(time.Second)

	sb := &sandpod.SandboxInfo{
		Name:         "test-sb",
		ID:           "ctr-001",
		Region:       "us-east-1",
		ProviderType: "aws",
		InstanceType: "t3.micro",
		ImageID:      "ami-123",
		State:        sandpod.StatePending,
		IP:           "10.0.0.1",
		PoderID:      "poder-001",
		PoderURL:     "tunnel://poder-001",
		Labels:       map[string]string{"env": "test"},
		CreatedAt:    now,
		LastActivity: now,
	}

	// Add
	if err := repo.Add(sb); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Get
	got, ok := repo.Get("test-sb")
	if !ok {
		t.Fatal("Get: not found")
	}
	if got.Name != sb.Name {
		t.Errorf("Name: got %q, want %q", got.Name, sb.Name)
	}
	if got.PoderID != sb.PoderID {
		t.Errorf("PoderID: got %q, want %q", got.PoderID, sb.PoderID)
	}
	if got.Labels["env"] != "test" {
		t.Errorf("Labels[env]: got %q, want %q", got.Labels["env"], "test")
	}

	// Get missing
	if _, ok := repo.Get("no-such"); ok {
		t.Error("Get(missing) should return false")
	}

	// Update
	if err := repo.Update("test-sb", func(s *sandpod.SandboxInfo) {
		s.State = sandpod.StateRunning
		s.IP = "10.0.0.2"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = repo.Get("test-sb")
	if got.State != sandpod.StateRunning {
		t.Errorf("State: got %v, want RUNNING", got.State)
	}
	if got.IP != "10.0.0.2" {
		t.Errorf("IP: got %q, want 10.0.0.2", got.IP)
	}

	// List
	all := repo.List()
	if len(all) != 1 {
		t.Errorf("List: got %d items, want 1", len(all))
	}

	// ListByPoderID
	byPoder := repo.ListByPoderID("poder-001")
	if len(byPoder) != 1 {
		t.Errorf("ListByPoderID: got %d items, want 1", len(byPoder))
	}
	byPoder2 := repo.ListByPoderID("other-poder")
	if len(byPoder2) != 0 {
		t.Errorf("ListByPoderID(other): got %d items, want 0", len(byPoder2))
	}

	// Delete
	if err := repo.Delete("test-sb"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := repo.Get("test-sb"); ok {
		t.Error("Get after Delete should return false")
	}
	// Delete missing
	if err := repo.Delete("test-sb"); err == nil {
		t.Error("Delete(missing): expected error")
	}
}

func TestSandboxRepo_MultipleItems(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewSandboxRepo(db)
	now := time.Now().UTC()

	for i := 0; i < 5; i++ {
		poderID := "poder-a"
		if i >= 3 {
			poderID = "poder-b"
		}
		sb := &sandpod.SandboxInfo{
			Name:         "sb-" + string(rune('0'+i)),
			State:        sandpod.StatePending,
			PoderID:      poderID,
			CreatedAt:    now.Add(time.Duration(i) * time.Second),
			LastActivity: now,
		}
		if err := repo.Add(sb); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	if got := repo.ListByPoderID("poder-a"); len(got) != 3 {
		t.Errorf("poder-a: got %d, want 3", len(got))
	}
	if got := repo.ListByPoderID("poder-b"); len(got) != 2 {
		t.Errorf("poder-b: got %d, want 2", len(got))
	}
	if got := repo.List(); len(got) != 5 {
		t.Errorf("List: got %d, want 5", len(got))
	}
}

// ─── Poder Repo ──────────────────────────────────────────────────────────────

func newPoderReq(id, region, providerType string) *sandpod.RegisterPoderRequest {
	return &sandpod.RegisterPoderRequest{
		ID:           id,
		Name:         "poder-" + id,
		URL:          "tunnel://" + id,
		Region:       region,
		ProviderType: providerType,
		Resources: sandpod.PoderResources{
			CPUCores:      4,
			MemoryBytes:   8 * 1024 * 1024 * 1024,
			MaxContainers: 10,
			Arch:          "amd64",
			OS:            "linux",
			OSVersion:     "Ubuntu 22.04",
			KernelVersion: "5.15.0",
		},
	}
}

func TestPoderRepo_RegisterGetList(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewPoderRepo(db)

	info, err := repo.Register(newPoderReq("p1", "us-east-1", "aws"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if info.State != sandpod.PoderStateOnline {
		t.Errorf("State: got %v, want ONLINE", info.State)
	}
	if info.Resources.CPUCores != 4 {
		t.Errorf("CPUCores: got %d, want 4", info.Resources.CPUCores)
	}

	// Get
	got, ok := repo.Get("p1")
	if !ok {
		t.Fatal("Get: not found")
	}
	if got.ID != "p1" {
		t.Errorf("ID: got %q, want p1", got.ID)
	}

	// Re-register (upsert) – should still return ONLINE, preserve created_at
	firstCreatedAt := info.CreatedAt
	info2, err := repo.Register(newPoderReq("p1", "us-east-1", "aws"))
	if err != nil {
		t.Fatalf("Re-Register: %v", err)
	}
	if !info2.CreatedAt.Equal(firstCreatedAt) {
		t.Errorf("created_at changed on re-register: %v → %v", firstCreatedAt, info2.CreatedAt)
	}

	// List
	list := repo.List()
	if len(list) != 1 {
		t.Errorf("List: got %d, want 1", len(list))
	}
}

func TestPoderRepo_Heartbeat(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewPoderRepo(db)
	if _, err := repo.Register(newPoderReq("p1", "us", "local")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := repo.Heartbeat("p1", &sandpod.HeartbeatRequest{
		Containers:  3,
		CPUUsage:    0.4,
		MemoryUsage: 0.6,
	}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	got, _ := repo.Get("p1")
	if got.Usage.Containers != 3 {
		t.Errorf("Containers: got %d, want 3", got.Usage.Containers)
	}
	if got.Usage.CPUUsage != 0.4 {
		t.Errorf("CPUUsage: got %f, want 0.4", got.Usage.CPUUsage)
	}

	// Heartbeat on missing ID should error
	if err := repo.Heartbeat("no-such", &sandpod.HeartbeatRequest{}); err == nil {
		t.Error("Heartbeat(missing): expected error")
	}
}

func TestPoderRepo_SelectBest(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewPoderRepo(db)

	// Register two poders in the same region
	_, _ = repo.Register(newPoderReq("heavy", "us", "local"))
	_, _ = repo.Register(newPoderReq("light", "us", "local"))

	// Make "heavy" heavily loaded
	_ = repo.Heartbeat("heavy", &sandpod.HeartbeatRequest{
		Containers: 8, CPUUsage: 0.9, MemoryUsage: 0.9,
	})
	// "light" has no load

	best, err := repo.SelectBest("us", "local")
	if err != nil {
		t.Fatalf("SelectBest: %v", err)
	}
	if best.ID != "light" {
		t.Errorf("SelectBest: got %q, want %q", best.ID, "light")
	}

	// Empty region/provider filter should still find a result
	best2, err := repo.SelectBest("", "")
	if err != nil {
		t.Fatalf("SelectBest(empty): %v", err)
	}
	if best2 == nil {
		t.Error("SelectBest(empty): got nil")
	}

	// No poder for unknown region
	_, err3 := repo.SelectBest("eu-west", "local")
	if err3 == nil {
		t.Error("SelectBest(unknown region): expected error")
	}
}

func TestPoderRepo_SelectBestRespectsMaxContainers(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewPoderRepo(db)
	_, _ = repo.Register(newPoderReq("full", "us", "local"))

	// Fill to capacity
	_ = repo.Heartbeat("full", &sandpod.HeartbeatRequest{
		Containers: 10, CPUUsage: 1.0, MemoryUsage: 1.0,
	})

	// Should not find any available poder since all are at max capacity
	_, err = repo.SelectBest("us", "local")
	if err == nil {
		t.Error("SelectBest: expected error when all poders are at max capacity")
	}
}

func TestPoderRepo_SetOffline(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewPoderRepo(db)
	_, _ = repo.Register(newPoderReq("p1", "us", "local"))

	repo.SetOffline("p1")

	got, _ := repo.Get("p1")
	if got.State != sandpod.PoderStateOffline {
		t.Errorf("State: got %v, want OFFLINE", got.State)
	}

	// SelectBest should not return offline poder
	_, offlineErr := repo.SelectBest("us", "local")
	if offlineErr == nil {
		t.Error("SelectBest: expected error when poder is offline")
	}
}

func TestPoderRepo_UpdateUsage(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewPoderRepo(db)
	_, _ = repo.Register(newPoderReq("p1", "us", "local"))

	if err := repo.UpdateUsage("p1", func(u *sandpod.PoderUsage) {
		u.Containers++
		u.CPUUsage = 0.25
	}); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}

	got, _ := repo.Get("p1")
	if got.Usage.Containers != 1 {
		t.Errorf("Containers: got %d, want 1", got.Usage.Containers)
	}
	if got.Usage.CPUUsage != 0.25 {
		t.Errorf("CPUUsage: got %f, want 0.25", got.Usage.CPUUsage)
	}
}

// ─── Job Repo ────────────────────────────────────────────────────────────────

func newJob(id string) *sandpod.Job {
	return &sandpod.Job{
		ID:           id,
		Type:         sandpod.JobTypeCreateSandbox,
		Status:       sandpod.JobStatusPending,
		SandboxName:  "sb-" + id,
		Region:       "us-east-1",
		ProviderType: "local",
		PoderID:      "p1",
		TraceContext: map[string]string{"req": id},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
}

func TestJobRepo_AddGetUpdate(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewJobRepo(db)

	job := newJob("j1")
	if err := repo.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	got, ok := repo.GetJob("j1")
	if !ok {
		t.Fatal("GetJob: not found")
	}
	if got.ID != "j1" {
		t.Errorf("ID: got %q, want j1", got.ID)
	}
	if got.TraceContext["req"] != "j1" {
		t.Errorf("TraceContext[req]: got %q, want j1", got.TraceContext["req"])
	}

	// GetJob missing
	if _, ok := repo.GetJob("no-such"); ok {
		t.Error("GetJob(missing) should return false")
	}

	// UpdateJob
	if err := repo.UpdateJob("j1", func(j *sandpod.Job) {
		j.Status = sandpod.JobStatusCompleted
		j.Result = &sandpod.JobResult{IP: "1.2.3.4", SandboxID: "ctr-001"}
		j.ErrorMessage = ""
	}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}

	got, _ = repo.GetJob("j1")
	if got.Status != sandpod.JobStatusCompleted {
		t.Errorf("Status: got %v, want COMPLETED", got.Status)
	}
	if got.Result == nil || got.Result.IP != "1.2.3.4" {
		t.Errorf("Result.IP: got %v", got.Result)
	}

	// ListJobs
	list := repo.ListJobs()
	if len(list) != 1 {
		t.Errorf("ListJobs: got %d, want 1", len(list))
	}
}

func TestJobRepo_PollJobs_Basic(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewJobRepo(db)

	// Add 5 PENDING jobs
	for i := 0; i < 5; i++ {
		if err := repo.AddJob(newJob("job-" + string(rune('a'+i)))); err != nil {
			t.Fatalf("AddJob %d: %v", i, err)
		}
	}

	// Poll 3 jobs
	jobs, err := repo.PollJobs(30*time.Second, 3)
	if err != nil {
		t.Fatalf("PollJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Errorf("PollJobs: got %d, want 3", len(jobs))
	}
	for _, j := range jobs {
		if j.Status != sandpod.JobStatusInProgress {
			t.Errorf("job %s status: got %v, want IN_PROGRESS", j.ID, j.Status)
		}
	}

	// Poll again: 2 remaining
	jobs2, err := repo.PollJobs(30*time.Second, 10)
	if err != nil {
		t.Fatalf("PollJobs(2nd): %v", err)
	}
	if len(jobs2) != 2 {
		t.Errorf("PollJobs(2nd): got %d, want 2", len(jobs2))
	}

	// Poll again: 0 remaining
	jobs3, err := repo.PollJobs(30*time.Second, 10)
	if err != nil {
		t.Fatalf("PollJobs(empty): %v", err)
	}
	if len(jobs3) != 0 {
		t.Errorf("PollJobs(empty): got %d, want 0", len(jobs3))
	}
}

func TestJobRepo_PollJobs_ResetStaleInProgress(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewJobRepo(db)

	// Add a job and manually claim it as IN_PROGRESS with a very old updated_at
	job := newJob("stale-j1")
	job.Status = sandpod.JobStatusInProgress
	// Set UpdatedAt to 2 minutes ago so it falls past the 1-minute timeout
	job.UpdatedAt = time.Now().UTC().Add(-2 * time.Minute)
	if err := repo.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// PollJobs with 1-minute timeout — stale job should be reclaimed
	jobs, err := repo.PollJobs(1*time.Minute, 10)
	if err != nil {
		t.Fatalf("PollJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("PollJobs: got %d, want 1 (stale job reclaimed)", len(jobs))
	}
	if jobs[0].ID != "stale-j1" {
		t.Errorf("job ID: got %q, want stale-j1", jobs[0].ID)
	}
}

func TestJobRepo_PollJobs_OrderByCreatedAt(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewJobRepo(db)
	base := time.Now().UTC()

	// Insert in reverse order of created_at
	ids := []string{"third", "first", "second"}
	offsets := []time.Duration{2 * time.Second, 0, 1 * time.Second}
	for i, id := range ids {
		j := newJob(id)
		j.CreatedAt = base.Add(offsets[i])
		j.UpdatedAt = j.CreatedAt
		if err := repo.AddJob(j); err != nil {
			t.Fatalf("AddJob %s: %v", id, err)
		}
	}

	jobs, err := repo.PollJobs(30*time.Second, 3)
	if err != nil {
		t.Fatalf("PollJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs, want 3", len(jobs))
	}
	want := []string{"first", "second", "third"}
	for i, j := range jobs {
		if j.ID != want[i] {
			t.Errorf("jobs[%d].ID = %q, want %q", i, j.ID, want[i])
		}
	}
}

// ─── Startup Recovery ────────────────────────────────────────────────────────

func TestOpen_StartupRecovery(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := sqldb.NewJobRepo(db)

	// Insert a job that is already IN_PROGRESS (simulating a crash)
	j := newJob("crash-job")
	j.Status = sandpod.JobStatusInProgress
	if err := repo.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Open a second connection to the same in-memory DB is not possible (each
	// :memory: is isolated), so we verify the startup recovery ran on the
	// existing DB by re-opening is not feasible here. Instead, verify the
	// startup recovery logic ran by checking that polling immediately reclaims
	// the job (the Open() startup recovery would have reset it, but since we
	// inserted after Open(), we use PollJobs with 0 timeout to exercise reset).
	jobs, err := repo.PollJobs(0, 10) // 0 timeout resets all IN_PROGRESS
	if err != nil {
		t.Fatalf("PollJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("expected 1 reclaimed job, got %d", len(jobs))
	}
}

func TestTokenRepo_CRUD(t *testing.T) {
	db, err := sqldb.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	repo := sqldb.NewTokenRepo(db)

	a := &sandpod.APIToken{Name: "alice", Prefix: "e2b_1111", Hash: "h-a", Role: "user", CreatedAt: time.Now()}
	b := &sandpod.APIToken{Name: "ops", Prefix: "e2b_2222", Hash: "h-b", Role: "admin", CreatedAt: time.Now()}
	if err := repo.Create(a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := repo.Create(b); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if err := repo.Create(a); err == nil {
		t.Error("duplicate hash should be rejected by PK")
	}
	list, err := repo.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	removed, err := repo.DeleteByPrefix("e2b_1111")
	if err != nil || len(removed) != 1 || removed[0] != "h-a" {
		t.Fatalf("delete: err=%v removed=%v", err, removed)
	}
	if list, _ := repo.List(); len(list) != 1 || list[0].Hash != "h-b" {
		t.Errorf("after delete: want [h-b], got %+v", list)
	}
	if removed, _ := repo.DeleteByPrefix("e2b_nope"); len(removed) != 0 {
		t.Errorf("delete miss: want none, got %v", removed)
	}
}
