// Copyright 2024 SandrPod
// Unit tests for Scheduler

package sandpod

import (
	"context"
	"errors"
	"testing"
)

// mockPoderRepo is a minimal implementation of PoderRepository for testing.
type mockPoderRepo struct {
	selectBestFn func(region, providerType string) (*PoderInfo, error)
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

	scheduler := NewScheduler(mock, "http://localhost:8080")

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
	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no available poder found")
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080")

	req := &CreateSandboxRequest{
		Name:         "my-sandbox",
		Region:       "us-east-1",
		ProviderType: "local",
	}

	_, err := scheduler.ScheduleSandboxCreation(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no local poder available, got nil")
	}

	expectedMsg := "no available local poder found"
	if err.Error() != expectedMsg {
		t.Errorf("expected error %q, got %q", expectedMsg, err.Error())
	}
}

func TestScheduler_ScheduleSandboxCreation_NoAvailableDockerPoder(t *testing.T) {
	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no available poder found")
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080")

	req := &CreateSandboxRequest{
		Name:         "my-sandbox",
		Region:       "us-east-1",
		ProviderType: "docker",
	}

	_, err := scheduler.ScheduleSandboxCreation(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no docker poder available, got nil")
	}

	expectedMsg := "no available docker poder found"
	if err.Error() != expectedMsg {
		t.Errorf("expected error %q, got %q", expectedMsg, err.Error())
	}
}

func TestScheduler_ScheduleSandboxCreation_EmptyProviderDefaultsToLocal(t *testing.T) {
	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no available poder found")
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080")

	req := &CreateSandboxRequest{
		Name:   "my-sandbox",
		Region: "us-east-1",
		// ProviderType intentionally empty
	}

	_, err := scheduler.ScheduleSandboxCreation(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty provider type defaulting to local, got nil")
	}

	expectedMsg := "no available local poder found"
	if err.Error() != expectedMsg {
		t.Errorf("expected error %q, got %q", expectedMsg, err.Error())
	}
}

func TestNewScheduler_DefaultAPIURL(t *testing.T) {
	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			return nil, errors.New("no poders")
		},
	}

	// Empty apiURL should use DefaultAPIURL
	scheduler := NewScheduler(mock, "")
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
	scheduler := NewScheduler(mock, customURL)
	if scheduler.apiURL != customURL {
		t.Errorf("expected custom API URL %s, got %s", customURL, scheduler.apiURL)
	}
}

func TestScheduler_SelectBestIsCalledWithCorrectArgs(t *testing.T) {
	var capturedRegion, capturedProviderType string

	mock := &mockPoderRepo{
		selectBestFn: func(region, providerType string) (*PoderInfo, error) {
			capturedRegion = region
			capturedProviderType = providerType
			return nil, errors.New("no poder")
		},
	}

	scheduler := NewScheduler(mock, "http://localhost:8080")
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
