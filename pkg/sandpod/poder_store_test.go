// Copyright 2024 SandrPod
// Unit tests for PoderStore

package sandpod

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newRegisterPoderRequest(id, name, region, providerType string, maxContainers int) *RegisterPoderRequest {
	return &RegisterPoderRequest{
		ID:           id,
		Name:         name,
		URL:          fmt.Sprintf("http://%s:8081", name),
		Region:       region,
		ProviderType: providerType,
		Resources: PoderResources{
			CPUCores:      4,
			MemoryBytes:   8 * 1024 * 1024 * 1024,
			MaxContainers: maxContainers,
			Arch:          "amd64",
			OS:            "linux",
		},
	}
}

func TestNewPoderStore(t *testing.T) {
	s := NewPoderStore()
	if s == nil {
		t.Fatal("NewPoderStore returned nil")
	}
	if len(s.List()) != 0 {
		t.Errorf("expected empty store, got %d items", len(s.List()))
	}
}

func TestPoderStore_Register(t *testing.T) {
	t.Run("creates new poder with ONLINE state", func(t *testing.T) {
		s := NewPoderStore()
		req := newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10)
		before := time.Now()

		poder, err := s.Register(req)
		after := time.Now()

		if err != nil {
			t.Fatalf("Register failed: %v", err)
		}
		if poder == nil {
			t.Fatal("Register returned nil poder")
		}
		if poder.ID != "p1" {
			t.Errorf("expected ID p1, got %s", poder.ID)
		}
		if poder.State != PoderStateOnline {
			t.Errorf("expected ONLINE state, got %s", poder.State)
		}
		if poder.CreatedAt.Before(before) || poder.CreatedAt.After(after) {
			t.Errorf("CreatedAt %v not within [%v, %v]", poder.CreatedAt, before, after)
		}
		if poder.Usage.Containers != 0 {
			t.Errorf("expected 0 containers, got %d", poder.Usage.Containers)
		}
		if poder.Resources.MaxContainers != 10 {
			t.Errorf("expected MaxContainers 10, got %d", poder.Resources.MaxContainers)
		}
	})

	t.Run("re-register upserts url and region but preserves CreatedAt", func(t *testing.T) {
		s := NewPoderStore()
		req := newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10)
		first, _ := s.Register(req)
		originalCreatedAt := first.CreatedAt

		// Small sleep to ensure time difference is detectable
		time.Sleep(2 * time.Millisecond)

		req2 := &RegisterPoderRequest{
			ID:           "p1",
			Name:         "poder-1-updated",
			URL:          "http://new-url:9090",
			Region:       "us-west-2",
			ProviderType: "local",
			Resources:    PoderResources{MaxContainers: 20},
		}
		updated, err := s.Register(req2)
		if err != nil {
			t.Fatalf("re-register failed: %v", err)
		}
		if updated.URL != "http://new-url:9090" {
			t.Errorf("expected updated URL, got %s", updated.URL)
		}
		if updated.Region != "us-west-2" {
			t.Errorf("expected updated region us-west-2, got %s", updated.Region)
		}
		if updated.CreatedAt != originalCreatedAt {
			t.Errorf("CreatedAt should be preserved: original %v, got %v", originalCreatedAt, updated.CreatedAt)
		}
		if updated.State != PoderStateOnline {
			t.Errorf("expected ONLINE state after re-register, got %s", updated.State)
		}

		// Verify only one entry in the store
		if len(s.List()) != 1 {
			t.Errorf("expected 1 item after re-register, got %d", len(s.List()))
		}
	})
}

func TestPoderStore_Heartbeat(t *testing.T) {
	t.Run("updates usage and lastHeartbeat", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10))

		before := time.Now()
		err := s.Heartbeat("p1", &HeartbeatRequest{
			Containers:  3,
			CPUUsage:    0.5,
			MemoryUsage: 0.4,
		})
		after := time.Now()

		if err != nil {
			t.Fatalf("Heartbeat failed: %v", err)
		}

		poder, ok := s.Get("p1")
		if !ok {
			t.Fatal("poder not found after heartbeat")
		}
		if poder.Usage.Containers != 3 {
			t.Errorf("expected 3 containers, got %d", poder.Usage.Containers)
		}
		if poder.Usage.CPUUsage != 0.5 {
			t.Errorf("expected CPUUsage 0.5, got %f", poder.Usage.CPUUsage)
		}
		if poder.Usage.MemoryUsage != 0.4 {
			t.Errorf("expected MemoryUsage 0.4, got %f", poder.Usage.MemoryUsage)
		}
		if poder.LastHeartbeat.Before(before) || poder.LastHeartbeat.After(after) {
			t.Errorf("LastHeartbeat not in expected range")
		}
		if poder.State != PoderStateOnline {
			t.Errorf("expected ONLINE state after heartbeat, got %s", poder.State)
		}
	})

	t.Run("missing id returns error", func(t *testing.T) {
		s := NewPoderStore()
		err := s.Heartbeat("nonexistent", &HeartbeatRequest{Containers: 1})
		if err == nil {
			t.Fatal("expected error for missing poder, got nil")
		}
	})
}

func TestPoderStore_Get(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10))

		poder, ok := s.Get("p1")
		if !ok {
			t.Fatal("expected to find p1")
		}
		if poder.ID != "p1" {
			t.Errorf("expected ID p1, got %s", poder.ID)
		}
	})

	t.Run("not found", func(t *testing.T) {
		s := NewPoderStore()
		_, ok := s.Get("nonexistent")
		if ok {
			t.Fatal("expected not found")
		}
	})
}

func TestPoderStore_List(t *testing.T) {
	t.Run("returns all items", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10))
		_, _ = s.Register(newRegisterPoderRequest("p2", "poder-2", "us-west-2", "aws", 20))
		_, _ = s.Register(newRegisterPoderRequest("p3", "poder-3", "eu-west-1", "aliyun", 15))

		items := s.List()
		if len(items) != 3 {
			t.Errorf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("empty store returns empty slice", func(t *testing.T) {
		s := NewPoderStore()
		items := s.List()
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})
}

func TestPoderStore_SelectBest(t *testing.T) {
	t.Run("returns least loaded poder", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p-high", "poder-high", "us-east-1", "local", 10))
		_, _ = s.Register(newRegisterPoderRequest("p-low", "poder-low", "us-east-1", "local", 10))

		// Set high load on p-high
		_ = s.Heartbeat("p-high", &HeartbeatRequest{Containers: 8, CPUUsage: 0.9, MemoryUsage: 0.8})
		// Set low load on p-low
		_ = s.Heartbeat("p-low", &HeartbeatRequest{Containers: 1, CPUUsage: 0.1, MemoryUsage: 0.1})

		best, err := s.SelectBest("us-east-1", "local")
		if err != nil {
			t.Fatalf("SelectBest failed: %v", err)
		}
		if best.ID != "p-low" {
			t.Errorf("expected p-low as best, got %s", best.ID)
		}
	})

	t.Run("empty region/providerType matches any", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10))

		best, err := s.SelectBest("", "")
		if err != nil {
			t.Fatalf("SelectBest with empty filters failed: %v", err)
		}
		if best == nil {
			t.Fatal("expected a poder, got nil")
		}
	})

	t.Run("skips OFFLINE poders", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p-offline", "poder-offline", "us-east-1", "local", 10))
		_, _ = s.Register(newRegisterPoderRequest("p-online", "poder-online", "us-east-1", "local", 10))

		s.SetOffline("p-offline")

		best, err := s.SelectBest("us-east-1", "local")
		if err != nil {
			t.Fatalf("SelectBest failed: %v", err)
		}
		if best.ID != "p-online" {
			t.Errorf("expected p-online, got %s", best.ID)
		}
	})

	t.Run("skips poders at max capacity", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p-full", "poder-full", "us-east-1", "local", 5))
		_, _ = s.Register(newRegisterPoderRequest("p-free", "poder-free", "us-east-1", "local", 10))

		// Fill p-full to capacity
		_ = s.Heartbeat("p-full", &HeartbeatRequest{Containers: 5})
		_ = s.Heartbeat("p-free", &HeartbeatRequest{Containers: 1})

		best, err := s.SelectBest("us-east-1", "local")
		if err != nil {
			t.Fatalf("SelectBest failed: %v", err)
		}
		if best.ID != "p-free" {
			t.Errorf("expected p-free, got %s", best.ID)
		}
	})

	t.Run("returns error when none available", func(t *testing.T) {
		s := NewPoderStore()
		// All offline
		_, _ = s.Register(newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10))
		s.SetOffline("p1")

		_, err := s.SelectBest("us-east-1", "local")
		if err == nil {
			t.Fatal("expected error when no poders available, got nil")
		}
	})

	t.Run("returns error on empty store", func(t *testing.T) {
		s := NewPoderStore()
		_, err := s.SelectBest("", "")
		if err == nil {
			t.Fatal("expected error on empty store, got nil")
		}
	})

	t.Run("region filter excludes non-matching", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p-east", "poder-east", "us-east-1", "local", 10))
		_, _ = s.Register(newRegisterPoderRequest("p-west", "poder-west", "us-west-2", "local", 10))

		best, err := s.SelectBest("us-west-2", "local")
		if err != nil {
			t.Fatalf("SelectBest failed: %v", err)
		}
		if best.ID != "p-west" {
			t.Errorf("expected p-west, got %s", best.ID)
		}
	})

	t.Run("providerType filter excludes non-matching", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p-local", "poder-local", "us-east-1", "local", 10))
		_, _ = s.Register(newRegisterPoderRequest("p-aws", "poder-aws", "us-east-1", "aws", 10))

		best, err := s.SelectBest("us-east-1", "aws")
		if err != nil {
			t.Fatalf("SelectBest failed: %v", err)
		}
		if best.ID != "p-aws" {
			t.Errorf("expected p-aws, got %s", best.ID)
		}
	})
}

func TestPoderStore_UpdateUsage(t *testing.T) {
	t.Run("applies mutation", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10))

		err := s.UpdateUsage("p1", func(usage *PoderUsage) {
			usage.Containers = 7
			usage.CPUUsage = 0.7
		})
		if err != nil {
			t.Fatalf("UpdateUsage failed: %v", err)
		}

		poder, _ := s.Get("p1")
		if poder.Usage.Containers != 7 {
			t.Errorf("expected 7 containers, got %d", poder.Usage.Containers)
		}
		if poder.Usage.CPUUsage != 0.7 {
			t.Errorf("expected CPUUsage 0.7, got %f", poder.Usage.CPUUsage)
		}
	})

	t.Run("missing returns error", func(t *testing.T) {
		s := NewPoderStore()
		err := s.UpdateUsage("nonexistent", func(usage *PoderUsage) {
			usage.Containers = 1
		})
		if err == nil {
			t.Fatal("expected error for missing poder, got nil")
		}
	})
}

func TestPoderStore_SetOffline(t *testing.T) {
	t.Run("marks poder as OFFLINE", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10))

		s.SetOffline("p1")

		poder, ok := s.Get("p1")
		if !ok {
			t.Fatal("poder not found after SetOffline")
		}
		if poder.State != PoderStateOffline {
			t.Errorf("expected OFFLINE state, got %s", poder.State)
		}
	})

	t.Run("no-op for nonexistent id", func(t *testing.T) {
		s := NewPoderStore()
		// Should not panic
		s.SetOffline("nonexistent")
	})
}

func TestPoderStore_Delete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		s := NewPoderStore()
		_, _ = s.Register(newRegisterPoderRequest("p1", "poder-1", "us-east-1", "local", 10))

		if err := s.Delete("p1"); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		_, ok := s.Get("p1")
		if ok {
			t.Fatal("expected p1 to be deleted")
		}
	})

	t.Run("missing returns error", func(t *testing.T) {
		s := NewPoderStore()
		err := s.Delete("nonexistent")
		if err == nil {
			t.Fatal("expected error for deleting nonexistent poder, got nil")
		}
	})
}

func TestPoderStore_Concurrency(t *testing.T) {
	s := NewPoderStore()
	const n = 30
	var wg sync.WaitGroup

	// Register concurrently (different IDs)
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			req := newRegisterPoderRequest(
				fmt.Sprintf("p%d", i),
				fmt.Sprintf("poder-%d", i),
				"us-east-1", "local", 10,
			)
			_, _ = s.Register(req)
		}()
	}
	wg.Wait()

	// Concurrent read/write
	wg.Add(n * 2)
	for i := range n {
		go func() {
			defer wg.Done()
			s.Get(fmt.Sprintf("p%d", i))
		}()
		go func() {
			defer wg.Done()
			_ = s.Heartbeat(fmt.Sprintf("p%d", i), &HeartbeatRequest{Containers: i % 10})
		}()
	}
	wg.Wait()
}
