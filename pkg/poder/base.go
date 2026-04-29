// Copyright 2024 SandrPod
// Poder abstraction layer implementation

package poder

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// BasePoder is the abstract base for Poder implementations.
// It provides common functionality; concrete types only need to implement provider-specific methods.
type BasePoder struct {
	name        string
	displayName string
	region      string
	mu          sync.RWMutex
	pods        map[string]*PodInfo
}

// NewBasePoder creates a new BasePoder with the given name, display name, and region.
func NewBasePoder(name, displayName, region string) *BasePoder {
	return &BasePoder{
		name:        name,
		displayName: displayName,
		region:      region,
		pods:        make(map[string]*PodInfo),
	}
}

func (p *BasePoder) Name() string {
	return p.name
}

func (p *BasePoder) DisplayName() string {
	return p.displayName
}

func (p *BasePoder) Region() string {
	return p.region
}

// ListPods returns all tracked pods (common implementation).
func (p *BasePoder) ListPods(ctx context.Context) ([]*PodInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pods := make([]*PodInfo, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod)
	}
	return pods, nil
}

// UpdatePodState updates the state of a tracked pod.
func (p *BasePoder) UpdatePodState(podID string, state PodState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if pod, ok := p.pods[podID]; ok {
		pod.State = state
	}
}

// GetPodByID retrieves a pod by ID (common implementation).
func (p *BasePoder) GetPodByID(podID string) (*PodInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pod, ok := p.pods[podID]
	return pod, ok
}

// GetPodByName retrieves a pod by name.
func (p *BasePoder) GetPodByName(name string) (*PodInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, pod := range p.pods {
		if pod.Name == name {
			return pod, true
		}
	}
	return nil, false
}

// RegisterPod adds a pod to the internal map.
func (p *BasePoder) RegisterPod(pod *PodInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.pods[pod.ID] = pod
}

// UnregisterPod removes a pod from the internal map.
func (p *BasePoder) UnregisterPod(podID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.pods[podID]; !ok {
		return fmt.Errorf("pod %s not found", podID)
	}
	delete(p.pods, podID)
	return nil
}

// DefaultWaitUntilRunning polls until the pod reaches the Running state or the timeout elapses.
func (p *BasePoder) DefaultWaitUntilRunning(ctx context.Context, podID string, timeout time.Duration) error {
	deadline, ok := ctx.Deadline()
	if ok {
		timeout = time.Until(deadline)
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("timeout waiting for pod %s to be running", podID)
		case <-ticker.C:
			pod, ok := p.GetPodByID(podID)
			if !ok {
				continue
			}
			if pod.State == PodStateRunning {
				return nil
			}
		}
	}
}
