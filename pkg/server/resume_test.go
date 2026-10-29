package server

import (
	"errors"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// A job that never reached the worker is re-delivered after the stale window —
// the case where the process died between accepting the request and sending it.
func TestResume_DeliversAStalePendingCreate(t *testing.T) {
	memory := store.NewMemoryStores()
	payload := []byte(`{"name":"lost-box","provider_type":"local"}`)
	job := &podpkg.Job{
		ID: "j1", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusPending,
		SandboxName: "lost-box", PoderID: "p1", Request: payload,
		CreatedAt: time.Now().Add(-2 * time.Minute), UpdatedAt: time.Now().Add(-2 * time.Minute),
	}
	if err := memory.Jobs.AddJob(job); err != nil {
		t.Fatal(err)
	}
	memory.Sandboxes.Add(&podpkg.SandboxInfo{Name: "lost-box", State: podpkg.StatePending})

	var got *podpkg.Job
	d := resumeDeps{
		jobs: memory.Jobs, sandboxes: memory.Sandboxes, tunnels: tunnel.NewTunnelStore(),
		deliver: func(j *podpkg.Job, req *podpkg.CreateSandboxRequest) error {
			got = j
			return nil
		},
	}
	if n := resumeOnce(d); n != 1 {
		t.Fatalf("delivered %d jobs, want 1", n)
	}
	if got == nil || got.ID != "j1" {
		t.Fatalf("delivered the wrong job: %+v", got)
	}
}

// A fresh job is left alone: the in-flight delivery gets its two minutes.
func TestResume_LeavesFreshJobsAlone(t *testing.T) {
	memory := store.NewMemoryStores()
	job := &podpkg.Job{
		ID: "j2", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusPending,
		SandboxName: "in-flight", PoderID: "p1", Request: []byte(`{"name":"in-flight"}`),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	_ = memory.Jobs.AddJob(job)
	delivered := false
	d := resumeDeps{
		jobs: memory.Jobs, sandboxes: memory.Sandboxes, tunnels: tunnel.NewTunnelStore(),
		deliver: func(*podpkg.Job, *podpkg.CreateSandboxRequest) error { delivered = true; return nil },
	}
	if n := resumeOnce(d); n != 0 || delivered {
		t.Fatal("re-delivered a job that was still in flight")
	}
}

// If the sandbox is already RUNNING the delivery succeeded and only the
// completion write was lost. Re-delivering would be harmless but wasteful; the
// job is closed instead.
func TestResume_ClosesJobsForRunningSandboxes(t *testing.T) {
	memory := store.NewMemoryStores()
	job := &podpkg.Job{
		ID: "j3", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusPending,
		SandboxName: "up-already", PoderID: "p1", Request: []byte(`{"name":"up-already"}`),
		CreatedAt: time.Now().Add(-5 * time.Minute), UpdatedAt: time.Now().Add(-5 * time.Minute),
	}
	_ = memory.Jobs.AddJob(job)
	memory.Sandboxes.Add(&podpkg.SandboxInfo{Name: "up-already", State: podpkg.StateRunning})

	delivered := false
	d := resumeDeps{
		jobs: memory.Jobs, sandboxes: memory.Sandboxes, tunnels: tunnel.NewTunnelStore(),
		deliver: func(*podpkg.Job, *podpkg.CreateSandboxRequest) error { delivered = true; return nil },
	}
	if n := resumeOnce(d); n != 0 || delivered {
		t.Fatal("re-delivered a create whose sandbox was already running")
	}
	if j, _ := memory.Jobs.GetJob("j3"); j.Status != podpkg.JobStatusCompleted {
		t.Fatalf("job status = %s, want COMPLETED", j.Status)
	}
}

// A job with no payload cannot be reconstructed. It must fail with a reason
// rather than sit PENDING forever.
func TestResume_FailsJobsWithoutPayload(t *testing.T) {
	memory := store.NewMemoryStores()
	job := &podpkg.Job{
		ID: "j4", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusPending,
		SandboxName: "no-payload", PoderID: "p1",
		CreatedAt: time.Now().Add(-5 * time.Minute), UpdatedAt: time.Now().Add(-5 * time.Minute),
	}
	_ = memory.Jobs.AddJob(job)
	d := resumeDeps{jobs: memory.Jobs, sandboxes: memory.Sandboxes, tunnels: tunnel.NewTunnelStore(),
		deliver: func(*podpkg.Job, *podpkg.CreateSandboxRequest) error { return nil }}
	resumeOnce(d)
	j, _ := memory.Jobs.GetJob("j4")
	if j.Status != podpkg.JobStatusFailed {
		t.Fatalf("status = %s, want FAILED", j.Status)
	}
}

// A worker that is not connected anywhere is a dead end: the job fails with the
// reason instead of retrying a target that no longer exists.
func TestDeliverCreate_NoTunnelAnywhereFails(t *testing.T) {
	memory := store.NewMemoryStores()
	job := &podpkg.Job{ID: "j5", SandboxName: "orphan", PoderID: "gone"}
	_ = memory.Jobs.AddJob(job)
	d := resumeDeps{
		jobs: memory.Jobs, sandboxes: memory.Sandboxes, tunnels: tunnel.NewTunnelStore(),
		cfg: Config{NodeURL: "http://self", Token: "t"}, owners: memory.TunnelOwners,
	}
	if err := deliverCreate(d, job, &podpkg.CreateSandboxRequest{Name: "orphan"}); !errors.Is(err, errNoWorker) {
		t.Fatalf("err = %v, want errNoWorker", err)
	}
	j, _ := memory.Jobs.GetJob("j5")
	if j.Status != podpkg.JobStatusFailed {
		t.Fatalf("status = %s, want FAILED", j.Status)
	}
}

// Two instances running the same pass must not both deliver: the claim is what
// makes at-least-once mean "one of us", not "every one of us".
func TestResume_OnlyOneInstanceClaims(t *testing.T) {
	memory := store.NewMemoryStores()
	job := &podpkg.Job{
		ID: "j-race", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusPending,
		SandboxName: "race", PoderID: "p1", Request: []byte(`{"name":"race"}`),
		CreatedAt: time.Now().Add(-5 * time.Minute), UpdatedAt: time.Now().Add(-5 * time.Minute),
	}
	_ = memory.Jobs.AddJob(job)
	memory.Sandboxes.Add(&podpkg.SandboxInfo{Name: "race", State: podpkg.StatePending})

	claims := 0
	d := resumeDeps{
		jobs: memory.Jobs, sandboxes: memory.Sandboxes, tunnels: tunnel.NewTunnelStore(),
		deliver: func(*podpkg.Job, *podpkg.CreateSandboxRequest) error { claims++; return nil },
	}
	// Two passes over the same stale row, as two instances would produce.
	resumeOnce(d)
	resumeOnce(d)
	if claims != 1 {
		t.Fatalf("delivered %d times, want exactly 1", claims)
	}
}

// A deleted sandbox must not be resurrected. Without this check the resumer
// delivered a create for a record that no longer existed, and the customer's
// deleted sandbox came back — repeatedly, on every pass.
// No record is not a verdict: the create path writes the job before the record, so
// a job with no record is a delivery that never got going — the case this pass
// exists for. The multi-instance check plants exactly this row.
//
// A deletion does not produce this state: the delete path fails the sandbox's
// pending jobs itself (see TestFailJobsFor_FinishesPendingCreatesOnly).
func TestResume_DeliversWhenRecordWasNeverWritten(t *testing.T) {
	memory := store.NewMemoryStores()
	job := &podpkg.Job{
		ID: "j-lost", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusPending,
		SandboxName: "never-delivered", PoderID: "p1",
		Request:   []byte(`{"name":"never-delivered"}`),
		CreatedAt: time.Now().Add(-5 * time.Minute), UpdatedAt: time.Now().Add(-5 * time.Minute),
	}
	_ = memory.Jobs.AddJob(job)
	delivered := false
	d := resumeDeps{
		jobs: memory.Jobs, sandboxes: memory.Sandboxes, tunnels: tunnel.NewTunnelStore(),
		deliver: func(*podpkg.Job, *podpkg.CreateSandboxRequest) error { delivered = true; return nil },
	}
	resumeOnce(d)
	if !delivered {
		t.Fatal("never delivered a create whose record was never written")
	}
}

// The delete path finishes the sandbox's pending creates, so the resumer cannot
// deliver one and resurrect what somebody removed.
func TestFailJobsFor_FinishesPendingCreatesOnly(t *testing.T) {
	memory := store.NewMemoryStores()
	add := func(id, sandbox string, status podpkg.JobStatus) {
		_ = memory.Jobs.AddJob(&podpkg.Job{
			ID: id, Type: podpkg.JobTypeCreateSandbox, Status: status, SandboxName: sandbox,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
	}
	add("j-pending", "gone-box", podpkg.JobStatusPending)
	add("j-other", "other-box", podpkg.JobStatusPending)
	add("j-done", "gone-box", podpkg.JobStatusCompleted)

	failJobsFor(memory.Jobs, "gone-box", "sandbox was deleted before its delivery completed")

	if j, _ := memory.Jobs.GetJob("j-pending"); j.Status != podpkg.JobStatusFailed {
		t.Errorf("pending create of the deleted sandbox = %s, want FAILED", j.Status)
	}
	if j, _ := memory.Jobs.GetJob("j-other"); j.Status != podpkg.JobStatusPending {
		t.Errorf("another sandbox's job was touched: %s", j.Status)
	}
	if j, _ := memory.Jobs.GetJob("j-done"); j.Status != podpkg.JobStatusCompleted {
		t.Errorf("a completed job was rewritten: %s", j.Status)
	}
}

// A sandbox that already failed stays failed: retrying cannot fix it, and a
// resumer that keeps trying is a loop with a log line per pass.
func TestResume_SkipsErroredSandboxes(t *testing.T) {
	memory := store.NewMemoryStores()
	job := &podpkg.Job{
		ID: "j-err", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusPending,
		SandboxName: "errored", PoderID: "p1", Request: []byte(`{"name":"errored"}`),
		CreatedAt: time.Now().Add(-5 * time.Minute), UpdatedAt: time.Now().Add(-5 * time.Minute),
	}
	_ = memory.Jobs.AddJob(job)
	memory.Sandboxes.Add(&podpkg.SandboxInfo{Name: "errored", State: podpkg.StateError})
	delivered := false
	d := resumeDeps{
		jobs: memory.Jobs, sandboxes: memory.Sandboxes, tunnels: tunnel.NewTunnelStore(),
		deliver: func(*podpkg.Job, *podpkg.CreateSandboxRequest) error { delivered = true; return nil },
	}
	resumeOnce(d)
	if delivered {
		t.Fatal("re-delivered a create for an ERROR sandbox")
	}
}
