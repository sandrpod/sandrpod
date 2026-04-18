// Copyright 2024 SandrPod
// SandPod 状态机实现

package sandpod

import (
	"fmt"
	"sync"
)

// StateMachine 状态机
type StateMachine struct {
	mu           sync.RWMutex
	state        State
	desiredState DesiredState
	transitions  map[State]map[State]bool
}

// NewStateMachine 创建状态机
func NewStateMachine(initial State, desired DesiredState) *StateMachine {
	sm := &StateMachine{
		state:        initial,
		desiredState: desired,
		transitions:  make(map[State]map[State]bool),
	}

	// 定义合法转换
	sm.addTransition(StatePending, StateStarting)
	sm.addTransition(StatePending, StateStopped)
	sm.addTransition(StatePending, StateTerminated)
	sm.addTransition(StateStarting, StateRunning)
	sm.addTransition(StateStarting, StateError)
	sm.addTransition(StateRunning, StateStopping)
	sm.addTransition(StateRunning, StateError)
	sm.addTransition(StateStopping, StateStopped)
	sm.addTransition(StateStopping, StateError)
	sm.addTransition(StateStopped, StateStarting)
	sm.addTransition(StateStopped, StateTerminated)
	sm.addTransition(StateError, StateStopping)
	sm.addTransition(StateError, StateTerminated)
	sm.addTransition(StateTerminated, StateTerminated)

	return sm
}

func (sm *StateMachine) addTransition(from, to State) {
	if sm.transitions[from] == nil {
		sm.transitions[from] = make(map[State]bool)
	}
	sm.transitions[from][to] = true
}

// CanTransition 检查是否可以转换
func (sm *StateMachine) CanTransition(to State) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.transitions[sm.state] == nil {
		return false
	}
	return sm.transitions[sm.state][to]
}

// Transition 转换状态
func (sm *StateMachine) Transition(to State) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Check transition validity without re-acquiring lock
	if sm.transitions[sm.state] == nil || !sm.transitions[sm.state][to] {
		return fmt.Errorf("invalid transition from %s to %s", sm.state, to)
	}

	sm.state = to
	return nil
}

// GetState 获取当前状态
func (sm *StateMachine) GetState() State {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state
}

// SetDesiredState 设置期望状态
func (sm *StateMachine) SetDesiredState(state DesiredState) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.desiredState = state
}

// GetDesiredState 获取期望状态
func (sm *StateMachine) GetDesiredState() DesiredState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.desiredState
}

// ShouldStop 检查是否应该停止
func (sm *StateMachine) ShouldStop() bool {
	return sm.desiredState == DesiredStateStopped || sm.desiredState == DesiredStateTerminate
}

// ShouldTerminate 检查是否应该终止
func (sm *StateMachine) ShouldTerminate() bool {
	return sm.desiredState == DesiredStateTerminate
}

// StateEvent 状态事件
type StateEvent string

const (
	EventStart       StateEvent = "START"
	EventStop        StateEvent = "STOP"
	EventDelete      StateEvent = "DELETE"
	EventTimeout     StateEvent = "TIMEOUT"
	EventError       StateEvent = "ERROR"
	EventReady       StateEvent = "READY"
	EventHealthOK    StateEvent = "HEALTH_OK"
	EventHealthError StateEvent = "HEALTH_ERROR"
)

// HandleEvent 处理事件
func (sm *StateMachine) HandleEvent(event StateEvent) error {
	// No lock here - caller holds the lock
	switch event {
	case EventStart:
		if sm.state == StatePending || sm.state == StateStopped {
			return sm.transitionUnlocked(StateStarting)
		}
	case EventStop:
		if sm.state == StateRunning {
			return sm.transitionUnlocked(StateStopping)
		}
	case EventDelete:
		if sm.state == StateStopped || sm.state == StateError {
			return sm.transitionUnlocked(StateTerminated)
		}
		if sm.state == StateRunning {
			return sm.transitionUnlocked(StateStopping)
		}
	case EventReady:
		if sm.state == StateStarting {
			return sm.transitionUnlocked(StateRunning)
		}
	case EventTimeout, EventError, EventHealthError:
		if sm.state == StateStarting || sm.state == StateRunning {
			return sm.transitionUnlocked(StateError)
		}
	case EventHealthOK:
		// 健康检查成功，不改变状态
	}

	return nil
}

// transitionUnlocked 不带锁的状态转换 (调用者必须持有锁)
func (sm *StateMachine) transitionUnlocked(to State) error {
	if sm.transitions[sm.state] == nil || !sm.transitions[sm.state][to] {
		return fmt.Errorf("invalid transition from %s to %s", sm.state, to)
	}
	sm.state = to
	return nil
}

// String 返回状态字符串
func (s State) String() string {
	return string(s)
}

func (d DesiredState) String() string {
	return string(d)
}
