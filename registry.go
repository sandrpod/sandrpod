// Copyright 2024 SandrPod
// SandPod 注册表 - 管理所有 SandPod 实例

package sandrpod

import (
	"context"
	"fmt"
	"sync"

	"github.com/sandrpod/sandrpod/pkg/poder"
	"github.com/sandrpod/sandrpod/pkg/sandpod"
)

// Registry SandPod 注册表
type Registry struct {
	mu    sync.RWMutex
	pods  map[string]*sandpod.BaseSandPod
	poder poder.Poder
}

// NewRegistry 创建注册表
func NewRegistry(p poder.Poder) *Registry {
	return &Registry{
		pods:  make(map[string]*sandpod.BaseSandPod),
		poder: p,
	}
}

// Create 创建 SandPod
func (r *Registry) Create(ctx context.Context, req *sandpod.CreateSandPodRequest) (*sandpod.SandPodInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 检查是否已存在
	if _, ok := r.pods[req.Name]; ok {
		return nil, fmt.Errorf("sandpod %s already exists", req.Name)
	}

	// 创建 SandPod
	sp := sandpod.NewBaseSandPod(req.Name, req.Name, r.poder)

	r.pods[req.Name] = sp

	// 启动
	if err := sp.Start(ctx); err != nil {
		delete(r.pods, req.Name)
		return nil, fmt.Errorf("failed to start sandpod: %w", err)
	}

	return sp.GetInfo(ctx)
}

// Get 获取 SandPod
func (r *Registry) Get(name string) (*sandpod.BaseSandPod, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sp, ok := r.pods[name]
	if !ok {
		return nil, fmt.Errorf("sandpod %s not found", name)
	}
	return sp, nil
}

// List 列出所有 SandPod
func (r *Registry) List() []*sandpod.SandPodInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	infos := make([]*sandpod.SandPodInfo, 0, len(r.pods))
	for _, sp := range r.pods {
		info, _ := sp.GetInfo(context.Background())
		if info != nil {
			infos = append(infos, info)
		}
	}
	return infos
}

// Stop 停止 SandPod
func (r *Registry) Stop(ctx context.Context, name string) error {
	r.mu.RLock()
	sp, ok := r.pods[name]
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("sandpod %s not found", name)
	}

	return sp.Stop(ctx)
}

// Start 启动 SandPod
func (r *Registry) Start(ctx context.Context, name string) error {
	r.mu.RLock()
	sp, ok := r.pods[name]
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("sandpod %s not found", name)
	}

	return sp.Start(ctx)
}

// Delete 删除 SandPod
func (r *Registry) Delete(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	sp, ok := r.pods[name]
	if !ok {
		return fmt.Errorf("sandpod %s not found", name)
	}

	if err := sp.Delete(ctx); err != nil {
		return err
	}

	delete(r.pods, name)
	return nil
}

// Process 执行代码
func (r *Registry) Process(ctx context.Context, name string, req *sandpod.ProcessRequest) (*sandpod.ProcessResult, error) {
	r.mu.RLock()
	sp, ok := r.pods[name]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("sandpod %s not found", name)
	}

	return sp.Process(ctx, req)
}
