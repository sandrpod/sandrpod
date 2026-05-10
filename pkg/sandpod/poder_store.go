// Copyright 2024 SandrPod
// Poder Store - in-memory Poder store

package sandpod

import (
	"fmt"
	"sync"
	"time"
)

// PoderInfo holds metadata for a registered Poder node.
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

// PoderState represents the online/offline state of a Poder node.
type PoderState string

const (
	PoderStateOnline  PoderState = "ONLINE"
	PoderStateOffline PoderState = "OFFLINE"
)

// PoderResources describes the hardware resources available on a Poder node.
type PoderResources struct {
	CPUCores      int    `json:"cpu_cores"`
	MemoryBytes   int64  `json:"memory_bytes"`
	MaxContainers int    `json:"max_containers"`
	Arch          string `json:"arch,omitempty"`       // e.g. amd64, arm64
	OS            string `json:"os,omitempty"`         // e.g. linux, darwin
	OSVersion     string `json:"os_version,omitempty"` // e.g. Ubuntu 22.04.3 LTS
	KernelVersion string `json:"kernel_version,omitempty"` // e.g. 5.15.0-91-generic
}

// PoderUsage holds current resource utilization for a Poder node.
type PoderUsage struct {
	Containers   int     `json:"containers"`    // current container count
	CPUUsage      float64 `json:"cpu_usage"`    // CPU utilization 0-1
	MemoryUsage   float64 `json:"memory_usage"` // memory utilization 0-1
}

// RegisterPoderRequest is the payload for registering a new Poder node.
type RegisterPoderRequest struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	Region       string `json:"region"`
	ProviderType string `json:"provider_type"` // aws, aliyun, local, docker
	Resources    PoderResources `json:"resources"`
}

// HeartbeatRequest carries usage stats sent by a Poder node on each heartbeat.
// ContainerNames is the authoritative list of sandbox container names currently
// running on this Poder; the Server uses it to reconcile RUNNING sandbox states.
type HeartbeatRequest struct {
	Containers     int      `json:"containers"`
	CPUUsage       float64  `json:"cpu_usage"`
	MemoryUsage    float64  `json:"memory_usage"`
	ContainerNames []string `json:"container_names,omitempty"`
}

// PoderStore is a thread-safe in-memory store for Poder node records.
type PoderStore struct {
	mu     sync.RWMutex
	poders map[string]*PoderInfo
}

// NewPoderStore creates a new in-memory PoderStore.
func NewPoderStore() *PoderStore {
	return &PoderStore{
		poders: make(map[string]*PoderInfo),
	}
}

// Register adds a new Poder node or updates an existing one with the same ID.
func (s *PoderStore) Register(req *RegisterPoderRequest) (*PoderInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update the existing record if already registered.
	if existing, ok := s.poders[req.ID]; ok {
		// Update the existing Poder entry.
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

// Heartbeat updates usage stats and refreshes the last-heartbeat timestamp for a Poder node.
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

// Get returns the PoderInfo for the given ID.
func (s *PoderStore) Get(id string) (*PoderInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	poder, ok := s.poders[id]
	return poder, ok
}

// List returns all registered Poder nodes.
func (s *PoderStore) List() []*PoderInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*PoderInfo, 0, len(s.poders))
	for _, poder := range s.poders {
		result = append(result, poder)
	}
	return result
}

// SelectBest returns the online Poder node with the lowest weighted load score,
// suitable for scheduling a new container. An empty providerType matches any type.
func (s *PoderStore) SelectBest(region, providerType string) (*PoderInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *PoderInfo
	var bestScore float64 = 999999 // lower is better

	for _, poder := range s.poders {
		// Skip offline nodes.
		if poder.State != PoderStateOnline {
			continue
		}

		// Filter by region when specified.
		if region != "" && poder.Region != region {
			continue
		}

		// Filter by provider type when specified.
		if providerType != "" && poder.ProviderType != providerType {
			continue
		}

		// Skip nodes that have reached their container limit.
		if poder.Usage.Containers >= poder.Resources.MaxContainers {
			continue
		}

		// Container ratio — lower container count means lower (better) score.
		containerScore := float64(poder.Usage.Containers) / float64(poder.Resources.MaxContainers)
		// CPU utilization.
		cpuScore := poder.Usage.CPUUsage
		// Memory utilization.
		memoryScore := poder.Usage.MemoryUsage

		// Weighted composite score.
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

// UpdateUsage atomically updates the usage stats for a Poder node via a callback.
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

// SetOffline marks a Poder node as offline.
func (s *PoderStore) SetOffline(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if poder, ok := s.poders[id]; ok {
		poder.State = PoderStateOffline
	}
}

// Delete removes the Poder record from the store.
func (s *PoderStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.poders[id]; !ok {
		return fmt.Errorf("poder %s not found", id)
	}
	delete(s.poders, id)
	return nil
}
