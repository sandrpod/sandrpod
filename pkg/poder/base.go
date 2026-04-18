// Copyright 2024 SandrPod
// Poder 抽象层实现

package poder

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// BasePoder Poder 抽象基类
// 提供通用功能，子类只需实现特定云厂商的方法
type BasePoder struct {
	name        string
	displayName string
	region      string
	mu          sync.RWMutex
	pods        map[string]*PodInfo
}

// NewBasePoder 创建基础 Poder
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

// ListPods 列出所有 Pod (通用实现)
func (p *BasePoder) ListPods(ctx context.Context) ([]*PodInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pods := make([]*PodInfo, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod)
	}
	return pods, nil
}

// UpdatePodState 更新 Pod 状态
func (p *BasePoder) UpdatePodState(podID string, state PodState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if pod, ok := p.pods[podID]; ok {
		pod.State = state
	}
}

// GetPodByID 获取 Pod (通用实现)
func (p *BasePoder) GetPodByID(podID string) (*PodInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pod, ok := p.pods[podID]
	return pod, ok
}

// GetPodByName 根据名称获取 Pod
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

// RegisterPod 注册 Pod
func (p *BasePoder) RegisterPod(pod *PodInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.pods[pod.ID] = pod
}

// UnregisterPod 注销 Pod
func (p *BasePoder) UnregisterPod(podID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.pods[podID]; !ok {
		return fmt.Errorf("pod %s not found", podID)
	}
	delete(p.pods, podID)
	return nil
}

// DefaultWaitUntilRunning 默认的等待 Running 实现
func (p *BasePoder) DefaultWaitUntilRunning(ctx context.Context, podID string, timeout time.Duration) error {
	deadline, ok := ctx.Deadline()
	if ok {
		timeout = deadline.Sub(time.Now())
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
