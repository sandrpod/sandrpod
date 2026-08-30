// Copyright 2026 SandrPod Contributors
// Unit tests for Scheduler

package sandpod

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockPoderRepo is a minimal implementation of PoderRepository for testing.
type mockPoderRepo struct {
	selectBestFn func(region, providerType string) (*PoderInfo, error)
	listFn       func() []*PoderInfo
}

func (m *mockPoderRepo) Register(req *RegisterPoderRequest) (*PoderInfo, error) {
	return nil, nil
}

func (m *mockPoderRepo) Heartbeat(id string, usage *HeartbeatRequest) error {
	return nil
}

func (m *mockPoderRepo) Get(id string) (*PoderInfo, bool) {
	return nil, false
}

func (m *mockPoderRepo) List() []*PoderInfo {
	if m.listFn != nil {
		return m.listFn()
	}
	return nil
}

func (m *mockPoderRepo) SelectBest(region, providerType string) (*PoderInfo, error) {
	return m.selectBestFn(region, providerType)
}

func (m *mockPoderRepo) UpdateUsage(id string, fn func(*PoderUsage)) error {
	return nil
}

func (m *mockPoderRepo) SetOffline(id string) {}

func (m *mockPoderRepo) Delete(id string) error {
	return nil
}

// Verify mockPoderRepo satisfies PoderRepository at compile time.
var _ PoderRepository = (*mockPoderRepo)(nil)

func TestShellQuoteSingleValue(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain string unchanged",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "string with single quote is escaped",
			input:    "it's",
			expected: "it'\\''s",
		},
		{
			name:     "empty string returns empty",
			input:    "",
			expected: "",
		},
		{
			name:     "multiple single quotes all escaped",
			input:    "it's a 'test'",
			expected: "it'\\''s a '\\''test'\\''",
		},
		{
			name:     "single quote only",
			input:    "'",
			expected: "'\\''",
		},
		{
			name:     "no special chars",
			input:    "http://localhost:8080",
			expected: "http://localhost:8080",
		},
		{
			name:     "url with path",
			input:    "https://api.example.com/v1",
			expected: "https://api.example.com/v1",
		},
		{
			name:     "string starting and ending with single quote",
			input:    "'wrapped'",
			expected: "'\\''wrapped'\\''",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shellQuoteSingleValue(tc.input)
			if got != tc.expected {
				t.Errorf("shellQuoteSingleValue(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestScheduler_ScheduleSandboxCreation_HappyPath(t *testing.T) {
	expectedPoder := &PoderInfo{
		ID:           "poder-abc",
		Name:         "poder-abc",
		URL:          "http://poder-abc:8081",
		Region:       "us-east-1",
		ProviderType: "local",
		State:        PoderStateOnline,
	}

	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return expectedPoder, nil
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080", "")

	req := &CreateSandboxRequest{
		Name:         "my-sandbox",
		Region:       "us-east-1",
		ProviderType: "local",
		InstanceType: "t3.micro",
		ImageID:      "sandrpod/toolbox:latest",
	}

	job, err := scheduler.ScheduleSandboxCreation(context.Background(), req)
	if err != nil {
		t.Fatalf("ScheduleSandboxCreation failed: %v", err)
	}
	if job == nil {
		t.Fatal("expected a job, got nil")
	}
	if job.PoderID != "poder-abc" {
		t.Errorf("expected PoderID poder-abc, got %s", job.PoderID)
	}
	if job.SandboxName != "my-sandbox" {
		t.Errorf("expected SandboxName my-sandbox, got %s", job.SandboxName)
	}
	if job.Region != "us-east-1" {
		t.Errorf("expected Region us-east-1, got %s", job.Region)
	}
	if job.ProviderType != "local" {
		t.Errorf("expected ProviderType local, got %s", job.ProviderType)
	}
	if job.Type != JobTypeCreateSandbox {
		t.Errorf("expected type CREATE_SANDBOX, got %s", job.Type)
	}
	if job.Status != JobStatusPending {
		t.Errorf("expected status PENDING, got %s", job.Status)
	}
	if job.ID == "" {
		t.Error("expected non-empty job ID")
	}
	if job.PoderURL != "http://poder-abc:8081" {
		t.Errorf("expected PoderURL http://poder-abc:8081, got %s", job.PoderURL)
	}
}

func TestScheduler_ScheduleSandboxCreation_NoAvailableLocalPoder(t *testing.T) {
	// The local path now waits for a worker that may still be registering,
	// so shorten the window rather than making the test sit through it.
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 200 * time.Millisecond
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no available poder found")
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080", "")

	req := &CreateSandboxRequest{
		Name:         "my-sandbox",
		Region:       "us-east-1",
		ProviderType: "local",
	}

	_, err := scheduler.ScheduleSandboxCreation(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no local poder available, got nil")
	}

	// Contains, not equals: the message now also says where to look.
	if !strings.Contains(err.Error(), "no available local poder found") {
		t.Errorf("error %q does not identify the missing worker", err.Error())
	}
}

func TestScheduler_ScheduleSandboxCreation_NoAvailableDockerPoder(t *testing.T) {
	// The local path now waits for a worker that may still be registering,
	// so shorten the window rather than making the test sit through it.
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 200 * time.Millisecond
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no available poder found")
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080", "")

	req := &CreateSandboxRequest{
		Name:         "my-sandbox",
		Region:       "us-east-1",
		ProviderType: "docker",
	}

	_, err := scheduler.ScheduleSandboxCreation(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no docker poder available, got nil")
	}

	if !strings.Contains(err.Error(), "no available docker poder found") {
		t.Errorf("error %q does not identify the missing worker", err.Error())
	}
}

func TestScheduler_ScheduleSandboxCreation_EmptyProviderDefaultsToLocal(t *testing.T) {
	// The local path now waits for a worker that may still be registering,
	// so shorten the window rather than making the test sit through it.
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 200 * time.Millisecond
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no available poder found")
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080", "")

	req := &CreateSandboxRequest{
		Name:   "my-sandbox",
		Region: "us-east-1",
		// ProviderType intentionally empty
	}

	_, err := scheduler.ScheduleSandboxCreation(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty provider type defaulting to local, got nil")
	}

	// Contains, not equals: the message now also says where to look.
	if !strings.Contains(err.Error(), "no available local poder found") {
		t.Errorf("error %q does not identify the missing worker", err.Error())
	}
}

func TestNewScheduler_DefaultAPIURL(t *testing.T) {
	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no poders")
		},
	}

	// Empty apiURL should use DefaultAPIURL
	scheduler := NewScheduler(mock, "", "")
	if scheduler.apiURL != DefaultAPIURL {
		t.Errorf("expected default API URL %s, got %s", DefaultAPIURL, scheduler.apiURL)
	}
}

func TestNewScheduler_CustomAPIURL(t *testing.T) {
	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no poders")
		},
	}

	customURL := "https://api.example.com"
	scheduler := NewScheduler(mock, customURL, "")
	if scheduler.apiURL != customURL {
		t.Errorf("expected custom API URL %s, got %s", customURL, scheduler.apiURL)
	}
}

func TestScheduler_SelectBestIsCalledWithCorrectArgs(t *testing.T) {
	// Exercises the no-worker path; keep it from sitting through the full
	// grace period.
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 200 * time.Millisecond
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	var capturedRegion, capturedProviderType string

	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			capturedRegion = region
			capturedProviderType = providerType
			return nil, errors.New("no poder")
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080", "")
	req := &CreateSandboxRequest{
		Name:         "test-sandbox",
		Region:       "ap-southeast-1",
		ProviderType: "local",
	}

	_, _ = scheduler.ScheduleSandboxCreation(context.Background(), req)

	if capturedRegion != "ap-southeast-1" {
		t.Errorf("SelectBest called with region %q, want %q", capturedRegion, "ap-southeast-1")
	}
	if capturedProviderType != "local" {
		t.Errorf("SelectBest called with providerType %q, want %q", capturedProviderType, "local")
	}
}

// A local worker cannot be provisioned on demand, but in the seconds after
// `docker compose up` it may simply not have registered yet — the control
// plane answers /health as soon as it binds. Failing immediately there turns a
// timing window into "this product is broken" for whoever is following the
// quickstart, which is the worst possible first impression and is not even
// accurate.
func TestScheduleLocal_WaitsForALateRegistration(t *testing.T) {
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 3 * time.Second
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	var calls atomic.Int32
	repo := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			// Absent for the first few polls, then registers.
			if calls.Add(1) < 3 {
				return nil, errors.New("no poder available")
			}
			return &PoderInfo{ID: "poder-late", State: PoderStateOnline, ProviderType: providerType}, nil
		},
	}

	s := NewScheduler(repo, "", "")
	job, err := s.ScheduleSandboxCreation(context.Background(),
		&CreateSandboxRequest{Name: "demo", ProviderType: "local"})
	if err != nil {
		t.Fatalf("a worker that registered during the grace period should have been used: %v", err)
	}
	if job.PoderID != "poder-late" {
		t.Errorf("scheduled on %q, want poder-late", job.PoderID)
	}
	if calls.Load() < 3 {
		t.Errorf("returned after %d checks — it cannot have waited", calls.Load())
	}
}

// A worker that is genuinely absent must still fail, and say something the
// reader can act on.
func TestScheduleLocal_StillFailsWhenNoWorkerAppears(t *testing.T) {
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 300 * time.Millisecond
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	repo := &mockPoderRepo{
		selectBestFn: func(string, string) (*PoderInfo, error) {
			return nil, errors.New("no poder available")
		},
	}
	s := NewScheduler(repo, "", "")
	_, err := s.ScheduleSandboxCreation(context.Background(),
		&CreateSandboxRequest{Name: "demo", ProviderType: "local"})
	if err == nil {
		t.Fatal("expected an error when no worker ever registers")
	}
	for _, want := range []string{"no available local poder", "docker compose ps"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — the reader needs somewhere to look", err, want)
		}
	}
}

// The common case must not pay for the grace period: a worker that is already
// there is returned without sleeping.
func TestScheduleLocal_NoDelayWhenWorkerPresent(t *testing.T) {
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 30 * time.Second
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	repo := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return &PoderInfo{ID: "poder-ready", State: PoderStateOnline, ProviderType: providerType}, nil
		},
	}
	s := NewScheduler(repo, "", "")
	start := time.Now()
	job, err := s.ScheduleSandboxCreation(context.Background(),
		&CreateSandboxRequest{Name: "demo", ProviderType: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if job.PoderID != "poder-ready" {
		t.Errorf("PoderID = %q", job.PoderID)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v with a worker already online — the grace period should cost nothing", elapsed)
	}
}

// A worker that is online but full is the steady-state failure of a busy
// deployment, and it used to be reported as the cold-start one: SelectBest
// filters full workers out, so the grace period ran to completion and then
// blamed a worker that `docker compose ps` shows running. Both halves matter —
// the message must name capacity, and it must not spend the grace period first,
// or every rejected create under load costs 30s.
func TestScheduleLocal_FullWorkerFailsFastAndSaysSo(t *testing.T) {
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 10 * time.Second
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	full := &PoderInfo{
		ID: "poder-full", State: PoderStateOnline, ProviderType: "local", Region: "local",
		Resources: PoderResources{MaxContainers: 10},
		Usage:     PoderUsage{Containers: 10},
	}
	repo := &mockPoderRepo{
		selectBestFn: func(string, string) (*PoderInfo, error) {
			return nil, errors.New("no poder available") // what SelectBest does with a full worker
		},
		listFn: func() []*PoderInfo { return []*PoderInfo{full} },
	}

	s := NewScheduler(repo, "", "")
	start := time.Now()
	_, err := s.ScheduleSandboxCreation(context.Background(),
		&CreateSandboxRequest{Name: "demo", ProviderType: "local", Region: "local"})
	if err == nil {
		t.Fatal("expected an error when the only worker is at its container limit")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v — waiting cannot free capacity, so it must not wait", elapsed)
	}
	if !strings.Contains(err.Error(), "container limit") {
		t.Errorf("error %q does not mention the container limit", err)
	}
	if strings.Contains(err.Error(), "docker compose ps") {
		t.Errorf("error %q still sends the reader to check a worker that is online", err)
	}
}

// The capacity check must not swallow the cold-start case it sits in front of:
// a worker below its limit that has not registered yet still gets waited for.
func TestScheduleLocal_UnfullWorkerDoesNotTriggerCapacityPath(t *testing.T) {
	prev := LocalPoderGracePeriod
	LocalPoderGracePeriod = 3 * time.Second
	t.Cleanup(func() { LocalPoderGracePeriod = prev })

	var calls atomic.Int32
	repo := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			if calls.Add(1) < 3 {
				return nil, errors.New("no poder available")
			}
			return &PoderInfo{ID: "poder-late", State: PoderStateOnline, ProviderType: providerType}, nil
		},
		// Present but with room — must not be counted as at-capacity.
		listFn: func() []*PoderInfo {
			return []*PoderInfo{{
				ID: "poder-roomy", State: PoderStateOnline, ProviderType: "local",
				Resources: PoderResources{MaxContainers: 10},
				Usage:     PoderUsage{Containers: 3},
			}}
		},
	}
	s := NewScheduler(repo, "", "")
	job, err := s.ScheduleSandboxCreation(context.Background(),
		&CreateSandboxRequest{Name: "demo", ProviderType: "local"})
	if err != nil {
		t.Fatalf("a worker with room must still go through the grace period: %v", err)
	}
	if job.PoderID != "poder-late" {
		t.Errorf("scheduled on %q, want poder-late", job.PoderID)
	}
}
