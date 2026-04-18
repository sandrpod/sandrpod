// Copyright 2024 SandrPod
// Sandbox Store - 内存中的 Sandbox 存储

package sandpod

import (
	"fmt"
	"sync"
	"time"
)

// SandboxInfo Sandbox 信息
type SandboxInfo struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Region        string            `json:"region"`
	ProviderType  string            `json:"provider_type,omitempty"`  // aws, aliyun, local
	InstanceType  string            `json:"instance_type"`
	ImageID       string            `json:"image_id,omitempty"`
	State         State             `json:"state"`
	IP            string            `json:"ip,omitempty"`
	PoderID       string            `json:"poder_id,omitempty"`        // 所属 Poder ID
	PoderURL      string            `json:"poder_url,omitempty"`        // Poder API URL
	ContainerID   string            `json:"container_id,omitempty"`    // 实际容器 ID
	ProxyURL      string            `json:"proxy_url,omitempty"`       // Toolbox Proxy URL
	APIURL        string            `json:"api_url,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	LastActivity  time.Time         `json:"last_activity"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// CreateSandboxRequest 创建 Sandbox 请求
type CreateSandboxRequest struct {
	Name         string `json:"name"`
	Region       string `json:"region"`
	ProviderType string `json:"provider_type"`  // aws, aliyun, local
	InstanceType string `json:"instance_type"`
	ImageID      string `json:"image_id,omitempty"`
}

// UpdateJobStatusRequest 更新任务状态请求
type UpdateJobStatusRequest struct {
	Status       JobStatus  `json:"status"`
	ErrorMessage string     `json:"error_message,omitempty"`
	Result       *JobResult `json:"result,omitempty"`
}

// ExecuteCodeRequest 执行代码请求
type ExecuteCodeRequest struct {
	Language string `json:"language"` // python, node, bash
	Code     string `json:"code"`
}

// SandboxStore Sandbox 存储
type SandboxStore struct {
	mu       sync.RWMutex
	sandboxes map[string]*SandboxInfo
}

// NewSandboxStore 创建 Sandbox 存储
func NewSandboxStore() *SandboxStore {
	return &SandboxStore{
		sandboxes: make(map[string]*SandboxInfo),
	}
}

// Add 添加 Sandbox
func (s *SandboxStore) Add(sb *SandboxInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.sandboxes[sb.Name]; exists {
		return fmt.Errorf("sandbox %s already exists", sb.Name)
	}

	s.sandboxes[sb.Name] = sb
	return nil
}

// Get 获取 Sandbox
func (s *SandboxStore) Get(name string) (*SandboxInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sb, ok := s.sandboxes[name]
	return sb, ok
}

// Update 更新 Sandbox
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

// List 列出所有 Sandbox
func (s *SandboxStore) List() []*SandboxInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*SandboxInfo, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		result = append(result, sb)
	}
	return result
}

// ListByPoderID 列出指定 Poder 的所有 Sandbox
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

// Delete 删除 Sandbox
func (s *SandboxStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sandboxes[name]; !ok {
		return fmt.Errorf("sandbox %s not found", name)
	}

	delete(s.sandboxes, name)
	return nil
}
