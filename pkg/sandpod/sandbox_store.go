// Copyright 2024 SandrPod
// Sandbox Store - in-memory sandbox storage

package sandpod

import (
	"fmt"
	"sync"
	"time"
)

// SandboxInfo holds metadata about a sandbox instance.
type SandboxInfo struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Region        string            `json:"region"`
	ProviderType  string            `json:"provider_type,omitempty"`  // provider type: aws, aliyun, local
	InstanceType  string            `json:"instance_type"`
	ImageID       string            `json:"image_id,omitempty"`
	State         State             `json:"state"`
	IP            string            `json:"ip,omitempty"`
	PoderID       string            `json:"poder_id,omitempty"`        // owning Poder ID
	PoderURL      string            `json:"poder_url,omitempty"`        // Poder API URL
	ContainerID   string            `json:"container_id,omitempty"`    // actual container ID
	ProxyURL      string            `json:"proxy_url,omitempty"`       // Toolbox proxy URL
	APIURL        string            `json:"api_url,omitempty"`
	// Runtime environment info (for AI-generated executable scripts)
	Arch          string            `json:"arch,omitempty"`       // e.g. amd64, arm64 (inherited from Poder host)
	OS            string            `json:"os,omitempty"`         // e.g. linux
	OSVersion     string            `json:"os_version,omitempty"` // e.g. Ubuntu 22.04.3 LTS
	CreatedAt     time.Time         `json:"created_at"`
	LastActivity  time.Time         `json:"last_activity"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// CreateSandboxRequest is the request body for creating a sandbox.
type CreateSandboxRequest struct {
	Name         string `json:"name"`
	Region       string `json:"region"`
	ProviderType string `json:"provider_type"`  // provider type: aws, aliyun, local
	InstanceType string `json:"instance_type"`
	ImageID      string `json:"image_id,omitempty"`
}

// UpdateJobStatusRequest is the request body for updating a job's status.
type UpdateJobStatusRequest struct {
	Status       JobStatus  `json:"status"`
	ErrorMessage string     `json:"error_message,omitempty"`
	Result       *JobResult `json:"result,omitempty"`
}

// ExecuteCodeRequest is the request body for executing code in a sandbox.
type ExecuteCodeRequest struct {
	Language string `json:"language"` // language: python, node, bash
	Code     string `json:"code"`
}

// SandboxStore is the in-memory sandbox store.
type SandboxStore struct {
	mu       sync.RWMutex
	sandboxes map[string]*SandboxInfo
}

// NewSandboxStore creates a new SandboxStore.
func NewSandboxStore() *SandboxStore {
	return &SandboxStore{
		sandboxes: make(map[string]*SandboxInfo),
	}
}

// Add inserts a new sandbox into the store.
func (s *SandboxStore) Add(sb *SandboxInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.sandboxes[sb.Name]; exists {
		return fmt.Errorf("sandbox %s already exists", sb.Name)
	}

	s.sandboxes[sb.Name] = sb
	return nil
}

// Get retrieves a sandbox by name.
func (s *SandboxStore) Get(name string) (*SandboxInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sb, ok := s.sandboxes[name]
	return sb, ok
}

// Update applies an update function to an existing sandbox.
func (s *SandboxStore) Update(name string, updateFn func(*SandboxInfo)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sb, ok := s.sandboxes[name]
	if !ok {
		return fmt.Errorf("sandbox %s not found", name)
	}

	updateFn(sb)
	return nil
}

// List returns all sandboxes in the store.
func (s *SandboxStore) List() []*SandboxInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*SandboxInfo, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		result = append(result, sb)
	}
	return result
}

// ListByPoderID returns all sandboxes belonging to the given Poder.
func (s *SandboxStore) ListByPoderID(poderID string) []*SandboxInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*SandboxInfo, 0)
	for _, sb := range s.sandboxes {
		if sb.PoderID == poderID {
			result = append(result, sb)
		}
	}
	return result
}

// Delete removes a sandbox from the store by name.
func (s *SandboxStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sandboxes[name]; !ok {
		return fmt.Errorf("sandbox %s not found", name)
	}

	delete(s.sandboxes, name)
	return nil
}
