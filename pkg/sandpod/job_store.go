// Copyright 2024 SandrPod
// Job Store - 内存中的任务存储

package sandpod

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// JobStore 任务存储
type JobStore struct {
	mu          sync.RWMutex
	jobs        map[string]*Job
	queue       []*Job // 按创建时间排序的任务队列
	jobTimeout  time.Duration // Job 超时时间，默认 5 分钟
}

// NewJobStore 创建任务存储
func NewJobStore() *JobStore {
	return &JobStore{
		jobs:       make(map[string]*Job),
		queue:      make([]*Job, 0),
		jobTimeout: 5 * time.Minute, // 默认 5 分钟超时
	}
}

// SetJobTimeout 设置 Job 超时时间
func (s *JobStore) SetJobTimeout(timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobTimeout = timeout
}

// AddJob 添加任务
func (s *JobStore) AddJob(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.jobs[job.ID]; exists {
		return fmt.Errorf("job %s already exists", job.ID)
	}

	s.jobs[job.ID] = job
	s.queue = append(s.queue, job)
	s.sortQueue()

	return nil
}

// GetJob 获取任务
func (s *JobStore) GetJob(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	job, ok := s.jobs[id]
	return job, ok
}

// UpdateJob 更新任务
func (s *JobStore) UpdateJob(id string, updateFn func(*Job)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}

	updateFn(job)
	job.UpdatedAt = time.Now()
	return nil
}

// PollJobs 轮询待处理的任务 (长轮询)
func (s *JobStore) PollJobs(timeout time.Duration, limit int) ([]*Job, error) {
	s.mu.Lock()

	// 先检查超时的 IN_PROGRESS 任务，恢复为 PENDING
	for _, job := range s.jobs {
		if job.Status == JobStatusInProgress {
			if time.Since(job.UpdatedAt) > s.jobTimeout {
				job.Status = JobStatusPending
				job.UpdatedAt = time.Now()
				log.Printf("Job %s timed out, resetting to PENDING", job.ID)
			}
		}
	}

	// 找出所有 PENDING 状态的任务
	pending := make([]*Job, 0)
	for _, job := range s.queue {
		if job.Status == JobStatusPending {
			pending = append(pending, job)
			if len(pending) >= limit {
				break
			}
		}
	}

	// 将这些任务标记为 IN_PROGRESS
	for _, job := range pending {
		job.Status = JobStatusInProgress
		job.UpdatedAt = time.Now()
	}

	s.mu.Unlock()

	return pending, nil
}

// ListJobs 列出所有任务
func (s *JobStore) ListJobs() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	jobs := make([]*Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	return jobs
}

// sortQueue 按创建时间排序
func (s *JobStore) sortQueue() {
	// 简单插入排序
	for i := 1; i < len(s.queue); i++ {
		for j := i; j > 0 && s.queue[j].CreatedAt.Before(s.queue[j-1].CreatedAt); j-- {
			s.queue[j], s.queue[j-1] = s.queue[j-1], s.queue[j]
		}
	}
}

// GenerateJobID 生成任务ID
func GenerateJobID() string {
	return fmt.Sprintf("job-%d-%s", time.Now().UnixNano(), randomString(8))
}

// randomString 生成随机字符串
func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
		time.Sleep(time.Nanosecond)
	}
	return string(b)
}
