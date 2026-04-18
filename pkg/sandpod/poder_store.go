// Copyright 2024 SandrPod
// Poder Store - 内存中的 Poder 存储

package sandpod

import (
	"fmt"
	"sync"
	"time"
)

// PoderInfo Poder 信息
type PoderInfo struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	URL           string                 `json:"url"`                   // Poder API URL (e.g., http://poder1:8081)
	Region        string                 `json:"region"`
	ProviderType  string                 `json:"provider_type"`        // aws, aliyun, local, docker
	State         PoderState             `json:"state"`
	Resources     PoderResources         `json:"resources"`
	Usage         PoderUsage             `json:"usage"`
	LastHeartbeat time.Time             `json:"last_heartbeat"`
	CreatedAt     time.Time             `json:"created_at"`
}

// PoderState Poder 状态
type PoderState string

const (
	PoderStateOnline  PoderState = "ONLINE"
	PoderStateOffline PoderState = "OFFLINE"
)

// PoderResources Poder 资源
type PoderResources struct {
	CPUCores      int   `json:"cpu_cores"`
	MemoryBytes   int64 `json:"memory_bytes"`
	MaxContainers int   `json:"max_containers"`
}

// PoderUsage Poder 当前使用情况
type PoderUsage struct {
	Containers   int     `json:"containers"`    // 当前容器数
	CPUUsage      float64 `json:"cpu_usage"`    // CPU 使用率 0-1
	MemoryUsage   float64 `json:"memory_usage"` // 内存使用率 0-1
}

// RegisterPoderRequest 注册 Poder 请求
type RegisterPoderRequest struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	Region       string `json:"region"`
	ProviderType string `json:"provider_type"` // aws, aliyun, local, docker
	Resources    PoderResources `json:"resources"`
}

// HeartbeatRequest 心跳请求
type HeartbeatRequest struct {
	Containers int     `json:"containers"`
	CPUUsage   float64 `json:"cpu_usage"`
	MemoryUsage float64 `json:"memory_usage"`
}

// PoderStore Poder 存储
type PoderStore struct {
	mu     sync.RWMutex
	poders map[string]*PoderInfo
}

// NewPoderStore 创建 Poder 存储
func NewPoderStore() *PoderStore {
	return &PoderStore{
		poders: make(map[string]*PoderInfo),
	}
}

// Register 注册 Poder
func (s *PoderStore) Register(req *RegisterPoderRequest) (*PoderInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 检查是否已存在
	if existing, ok := s.poders[req.ID]; ok {
		// 更新现有 Poder
		existing.URL = req.URL
		existing.Region = req.Region
		existing.Resources = req.Resources
		existing.LastHeartbeat = time.Now()
		existing.State = PoderStateOnline
		return existing, nil
	}

	poder := &PoderInfo{
		ID:           req.ID,
		Name:         req.Name,
		URL:          req.URL,
		Region:       req.Region,
		ProviderType: req.ProviderType,
		State:        PoderStateOnline,
		Resources:    req.Resources,
		Usage: PoderUsage{
			Containers: 0,
			CPUUsage:   0,
			MemoryUsage: 0,
		},
		LastHeartbeat: time.Now(),
		CreatedAt:     time.Now(),
	}

	s.poders[req.ID] = poder
	return poder, nil
}

// Heartbeat 更新心跳
func (s *PoderStore) Heartbeat(id string, usage *HeartbeatRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	poder, ok := s.poders[id]
	if !ok {
		return fmt.Errorf("poder %s not found", id)
	}

	poder.Usage = PoderUsage{
		Containers:  usage.Containers,
		CPUUsage:   usage.CPUUsage,
		MemoryUsage: usage.MemoryUsage,
	}
	poder.LastHeartbeat = time.Now()
	poder.State = PoderStateOnline

	return nil
}

// Get 获取 Poder
func (s *PoderStore) Get(id string) (*PoderInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	poder, ok := s.poders[id]
	return poder, ok
}

// List 列出所有 Poder
func (s *PoderStore) List() []*PoderInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*PoderInfo, 0, len(s.poders))
	for _, poder := range s.poders {
		result = append(result, poder)
	}
	return result
}

// SelectBest 选择负载最低的 Poder（用于创建新容器）
// 如果 providerType 为空，则不限制类型
func (s *PoderStore) SelectBest(region, providerType string) (*PoderInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *PoderInfo
	var bestScore float64 = 999999 // 越低越好

	for _, poder := range s.poders {
		// 跳过离线的 Poder
		if poder.State != PoderStateOnline {
			continue
		}

		// 如果指定了 region，优先选择同区域的
		if region != "" && poder.Region != region {
			continue
		}

		// 如果指定了 providerType，匹配类型
		if providerType != "" && poder.ProviderType != providerType {
			continue
		}

		// 跳过资源已满的
		if poder.Usage.Containers >= poder.Resources.MaxContainers {
			continue
		}

		// 计算负载分数: 容器数越少分数越低（越优先）
		containerScore := float64(poder.Usage.Containers) / float64(poder.Resources.MaxContainers)
		// CPU 使用率
		cpuScore := poder.Usage.CPUUsage
		// 内存使用率
		memoryScore := poder.Usage.MemoryUsage

		// 综合分数 (加权)
		totalScore := containerScore*0.6 + cpuScore*0.2 + memoryScore*0.2

		if totalScore < bestScore {
			bestScore = totalScore
			best = poder
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no available poder found")
	}

	return best, nil
}

// UpdateUsage 更新使用情况
func (s *PoderStore) UpdateUsage(id string, fn func(*PoderUsage)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	poder, ok := s.poders[id]
	if !ok {
		return fmt.Errorf("poder %s not found", id)
	}

	fn(&poder.Usage)
	return nil
}

// SetOffline 设置 Poder 离线
func (s *PoderStore) SetOffline(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if poder, ok := s.poders[id]; ok {
		poder.State = PoderStateOffline
	}
}
