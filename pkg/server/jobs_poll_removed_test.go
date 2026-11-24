package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// The job poll is gone. It used to hand any worker credential every tenant's
// pending create requests (env vars included) and flip their statuses: PENDING
// to IN_PROGRESS, which hid them from the outbox resumer, and in-flight
// IN_PROGRESS back to PENDING. The old path must now neither return jobs nor
// change one, whoever asks.
func TestJobPollRemoved(t *testing.T) {
	stores := store.NewMemoryStores()
	old := time.Now().Add(-time.Hour)
	for _, j := range []*podpkg.Job{
		{ID: "pending-1", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusPending,
			Owner: "tenant-b", Request: []byte(`{"env_vars":{"API_KEY":"secret"}}`), CreatedAt: old, UpdatedAt: old},
		{ID: "running-1", Type: podpkg.JobTypeCreateSandbox, Status: podpkg.JobStatusInProgress,
			Owner: "tenant-b", CreatedAt: old, UpdatedAt: old},
	} {
		if err := stores.Jobs.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Config{Token: "admin-tok"}, stores, tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/poll", nil)
	req.Header.Set("X-Sandrpod-Token", "admin-tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/v1/jobs/poll: status %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}

	for id, want := range map[string]podpkg.JobStatus{
		"pending-1": podpkg.JobStatusPending,
		"running-1": podpkg.JobStatusInProgress,
	} {
		j, ok := stores.Jobs.GetJob(id)
		if !ok {
			t.Fatalf("job %s disappeared", id)
		}
		if j.Status != want {
			t.Errorf("job %s: status %s after the call, want %s unchanged", id, j.Status, want)
		}
	}
}
