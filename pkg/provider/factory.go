// Copyright 2024 SandrPod
// Provider 工厂 - 动态注册和获取 Provider

package provider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Factory Provider 工厂
type Factory struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// 全局工厂实例
var globalFactory *Factory
var initOnce sync.Once

// GetFactory 获取全局工厂实例
func GetFactory() *Factory {
	initOnce.Do(func() {
		globalFactory = &Factory{
			providers: make(map[string]Provider),
		}
	})
	return globalFactory
}

// Register 注册 Provider
func (f *Factory) Register(p Provider) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if p == nil {
		return fmt.Errorf("provider cannot be nil")
	}

	name := p.Name()
	if name == "" {
		return fmt.Errorf("provider name cannot be empty")
	}

	if _, exists := f.providers[name]; exists {
		return fmt.Errorf("provider %s already registered", name)
	}

	f.providers[name] = p
	return nil
}

// Get 获取 Provider
func (f *Factory) Get(name string) (Provider, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	p, ok := f.providers[name]
	if !ok {
		return nil, fmt.Errorf("provider %s not found", name)
	}
	return p, nil
}

// MustGet 获取 Provider，如果不存在 panic
func (f *Factory) MustGet(name string) Provider {
	p, err := f.Get(name)
	if err != nil {
		panic(err)
	}
	return p
}

// List 列出所有已注册的 Provider
func (f *Factory) List() []Provider {
	f.mu.RLock()
	defer f.mu.RUnlock()

	providers := make([]Provider, 0, len(f.providers))
	for _, p := range f.providers {
		providers = append(providers, p)
	}
	return providers
}

// Names 列出所有 Provider 名称
func (f *Factory) Names() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	names := make([]string, 0, len(f.providers))
	for name := range f.providers {
		names = append(names, name)
	}
	return names
}

// Unregister 注销 Provider
func (f *Factory) Unregister(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.providers[name]; !exists {
		return fmt.Errorf("provider %s not found", name)
	}
	delete(f.providers, name)
	return nil
}

// RegisterFunc 便捷的注册函数
func RegisterFunc(name, displayName string, newFunc func() Provider) error {
	return GetFactory().Register(&wrapperProvider{
		name:        name,
		displayName: displayName,
		newFunc:     newFunc,
	})
}

// wrapperProvider 包装器
type wrapperProvider struct {
	name        string
	displayName string
	newFunc     func() Provider
	instance    Provider
	mu          sync.Once
}

func (w *wrapperProvider) Name() string {
	return w.name
}

func (w *wrapperProvider) DisplayName() string {
	return w.displayName
}

func (w *wrapperProvider) ensureInstance() {
	w.mu.Do(func() {
		w.instance = w.newFunc()
	})
}

func (w *wrapperProvider) CreateVM(ctx context.Context, req *CreateVMRequest) (*VMInfo, error) {
	w.ensureInstance()
	return w.instance.CreateVM(ctx, req)
}

func (w *wrapperProvider) DeleteVM(ctx context.Context, vmID string) error {
	w.ensureInstance()
	return w.instance.DeleteVM(ctx, vmID)
}

func (w *wrapperProvider) GetVM(ctx context.Context, vmID string) (*VMInfo, error) {
	w.ensureInstance()
	return w.instance.GetVM(ctx, vmID)
}

func (w *wrapperProvider) ListVMs(ctx context.Context) ([]*VMInfo, error) {
	w.ensureInstance()
	return w.instance.ListVMs(ctx)
}

func (w *wrapperProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*CommandResult, error) {
	w.ensureInstance()
	return w.instance.ExecuteCommand(ctx, vmID, command)
}

func (w *wrapperProvider) WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error {
	w.ensureInstance()
	return w.instance.WaitUntilRunning(ctx, vmID, timeout)
}

func (w *wrapperProvider) GetHealthStatus(ctx context.Context, vmID string) (*HealthStatus, error) {
	w.ensureInstance()
	return w.instance.GetHealthStatus(ctx, vmID)
}

func (w *wrapperProvider) ListRegions(ctx context.Context) ([]string, error) {
	w.ensureInstance()
	return w.instance.ListRegions(ctx)
}

func (w *wrapperProvider) ListInstanceTypes(ctx context.Context, region string) ([]*InstanceType, error) {
	w.ensureInstance()
	return w.instance.ListInstanceTypes(ctx, region)
}

func (w *wrapperProvider) GetDefaultImage(ctx context.Context, region string) (string, error) {
	w.ensureInstance()
	return w.instance.GetDefaultImage(ctx, region)
}

func (w *wrapperProvider) Cleanup(ctx context.Context) error {
	w.ensureInstance()
	return w.instance.Cleanup(ctx)
}
