// Copyright 2024 SandrPod
// SandPod 抽象实现

package sandpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sandrpod/sandrpod/pkg/poder"
)

// BaseSandPod 基础 SandPod 实现
type BaseSandPod struct {
	mu       sync.RWMutex
	id       string
	name     string
	info     *SandPodInfo
	poder    poder.Poder
	stateMachine *StateMachine
	httpClient   *http.Client
}

// NewBaseSandPod 创建 BaseSandPod
func NewBaseSandPod(id, name string, p poder.Poder) *BaseSandPod {
	sp := &BaseSandPod{
		id:       id,
		name:     name,
		poder:    p,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		stateMachine: NewStateMachine(StatePending, DesiredStateRunning),
		info: &SandPodInfo{
			ID:            id,
			Name:          name,
			DesiredState:  DesiredStateRunning,
			Labels:        make(map[string]string),
		},
	}
	return sp
}

func (sp *BaseSandPod) ID() string {
	return sp.id
}

func (sp *BaseSandPod) Name() string {
	return sp.name
}

func (sp *BaseSandPod) GetState() State {
	return sp.stateMachine.GetState()
}

func (sp *BaseSandPod) GetDesiredState() DesiredState {
	return sp.stateMachine.GetDesiredState()
}

func (sp *BaseSandPod) SetDesiredState(state DesiredState) error {
	sp.stateMachine.SetDesiredState(state)

	switch state {
	case DesiredStateRunning:
		return sp.stateMachine.HandleEvent(EventStart)
	case DesiredStateStopped:
		return sp.stateMachine.HandleEvent(EventStop)
	case DesiredStateTerminate:
		return sp.stateMachine.HandleEvent(EventDelete)
	}

	return nil
}

// Start 启动 SandPod
func (sp *BaseSandPod) Start(ctx context.Context) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	// 检查容器是否已存在但处于停止状态
	existingPod, err := sp.poder.GetPod(ctx, sp.id)
	if err == nil && existingPod != nil && existingPod.State == poder.PodStateStopped {
		// 容器已存在但停止了 - 恢复它
		if err := sp.stateMachine.HandleEvent(EventStart); err != nil {
			return err
		}

		if err := sp.poder.UnpausePod(ctx, sp.id); err != nil {
			sp.stateMachine.HandleEvent(EventError)
			return fmt.Errorf("failed to unpause pod: %w", err)
		}

		if err := sp.poder.WaitUntilRunning(ctx, sp.id, 5*time.Minute); err != nil {
			sp.stateMachine.HandleEvent(EventTimeout)
			return fmt.Errorf("failed to wait for running: %w", err)
		}

		sp.stateMachine.HandleEvent(EventReady)
		sp.info.State = StateRunning
		return nil
	}

	// 容器不存在 - 创建新的
	if err := sp.stateMachine.HandleEvent(EventStart); err != nil {
		return err
	}

	// 调用 Poder 创建 Pod
	podInfo, err := sp.poder.CreatePod(ctx, &poder.CreatePodRequest{
		Name:         sp.name,
		Region:       sp.info.Region,
		InstanceType: sp.info.InstanceType,
		ImageID:      sp.info.ImageID,
		Provider:     sp.info.Provider,
		Labels:      sp.info.Labels,
		APIURL:       sp.info.APIURL,
	})
	if err != nil {
		sp.stateMachine.HandleEvent(EventError)
		return fmt.Errorf("failed to create pod: %w", err)
	}

	// 更新信息
	sp.info.IP = podInfo.IP
	sp.info.Region = podInfo.Region
	sp.info.Provider = podInfo.Provider
	sp.info.InstanceType = podInfo.InstanceType
	sp.info.CreatedAt = podInfo.CreatedAt

	// 等待 Running (使用 podInfo.ID，实际的容器ID)
	if err := sp.poder.WaitUntilRunning(ctx, podInfo.ID, 5*time.Minute); err != nil {
		sp.stateMachine.HandleEvent(EventTimeout)
		return fmt.Errorf("failed to wait for running: %w", err)
	}

	sp.stateMachine.HandleEvent(EventReady)
	sp.info.State = StateRunning

	return nil
}

// Stop 停止 SandPod
func (sp *BaseSandPod) Stop(ctx context.Context) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if err := sp.stateMachine.HandleEvent(EventStop); err != nil {
		return err
	}

	// 使用 PausePod 暂停容器而不是删除
	if err := sp.poder.PausePod(ctx, sp.id); err != nil {
		sp.stateMachine.HandleEvent(EventError)
		return fmt.Errorf("failed to stop pod: %w", err)
	}

	sp.info.State = StateStopped
	return nil
}

// Delete 删除 SandPod
func (sp *BaseSandPod) Delete(ctx context.Context) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	// 先停止
	if sp.GetState() == StateRunning {
		if err := sp.poder.DeletePod(ctx, sp.id); err != nil {
			// 忽略错误，继续删除
		}
	}

	if err := sp.stateMachine.HandleEvent(EventDelete); err != nil {
		return err
	}

	sp.info.State = StateTerminated
	return nil
}

// Process 执行代码
func (sp *BaseSandPod) Process(ctx context.Context, req *ProcessRequest) (*ProcessResult, error) {
	sp.mu.RLock()
	state := sp.GetState()
	sp.mu.RUnlock()

	if state != StateRunning {
		return nil, fmt.Errorf("sandpod is not running, current state: %s", state)
	}

	// 获取 Toolbox API URL
	sp.mu.RLock()
	apiURL := sp.info.APIURL
	sp.mu.RUnlock()

	if apiURL == "" {
		return nil, fmt.Errorf("toolbox api url not available")
	}

	// 构建请求
	toolboxReq := map[string]interface{}{
		"language": req.Lang,
		"code":     req.Code,
		"timeout":  req.Timeout,
	}

	body, err := json.Marshal(toolboxReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// 发送请求
	httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL+"/process", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := sp.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute: %w", err)
	}
	defer resp.Body.Close()

	// 解析响应
	var result ProcessResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// GetHealthStatus 获取健康状态
func (sp *BaseSandPod) GetHealthStatus(ctx context.Context) (*HealthStatus, error) {
	status, err := sp.poder.GetHealthStatus(ctx, sp.id)
	if err != nil {
		return nil, err
	}
	return &HealthStatus{
		PodReady:     status.PodReady,
		DockerReady:  status.DockerReady,
		ToolboxReady: status.ToolboxReady,
		APIReachable: status.APIReachable,
	}, nil
}

// GetInfo 获取信息
func (sp *BaseSandPod) GetInfo(ctx context.Context) (*SandPodInfo, error) {
	sp.mu.RLock()
	defer sp.mu.RUnlock()

	// 从 Poder 获取最新状态
	podInfo, err := sp.poder.GetPod(ctx, sp.id)
	if err == nil {
		switch podInfo.State {
		case poder.PodStateRunning:
			sp.info.State = StateRunning
		case poder.PodStateStopping:
			sp.info.State = StateStopping
		case poder.PodStateStopped:
			sp.info.State = StateStopped
		case poder.PodStateError:
			sp.info.State = StateError
		}
		sp.info.IP = podInfo.IP
		sp.info.LastActivity = podInfo.LastActivity
	}

	infoCopy := *sp.info
	return &infoCopy, nil
}
