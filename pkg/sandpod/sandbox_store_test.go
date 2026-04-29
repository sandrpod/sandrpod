// Copyright 2024 SandrPod
// Unit tests for SandboxStore

package sandpod

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestSandbox(name, poderID string, state State) *SandboxInfo {
	return &SandboxInfo{
		ID:           fmt.Sprintf("id-%s", name),
		Name:         name,
		Region:       "us-east-1",
		ProviderType: "local",
		InstanceType: "t3.micro",
		ImageID:      "ami-123",
		State:        state,
		IP:           "10.0.0.1",
		PoderID:      poderID,
		PoderURL:     "http://poder:8080",
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
		Labels:       map[string]string{"env": "test"},
	}
}

func TestNewSandboxStore(t *testing.T) {
	s := NewSandboxStore()
	if s == nil {
		t.Fatal("NewSandboxStore returned nil")
	}
	items := s.List()
	if len(items) != 0 {
		t.Errorf("expected empty store, got %d items", len(items))
	}
}

func TestSandboxStore_Add(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		s := NewSandboxStore()
		sb := newTestSandbox("sb1", "poder1", StatePending)
		if err := s.Add(sb); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
		if len(s.List()) != 1 {
			t.Errorf("expected 1 item, got %d", len(s.List()))
		}
	})

	t.Run("duplicate returns error", func(t *testing.T) {
		s := NewSandboxStore()
		sb := newTestSandbox("sb1", "poder1", StatePending)
		if err := s.Add(sb); err != nil {
			t.Fatalf("first Add failed: %v", err)
		}
		err := s.Add(sb)
		if err == nil {
			t.Fatal("expected error for duplicate add, got nil")
		}
	})

	t.Run("multiple distinct items", func(t *testing.T) {
		s := NewSandboxStore()
		for i := range 5 {
			sb := newTestSandbox(fmt.Sprintf("sb%d", i), "poder1", StatePending)
			if err := s.Add(sb); err != nil {
				t.Fatalf("Add(%d) failed: %v", i, err)
			}
		}
		if len(s.List()) != 5 {
			t.Errorf("expected 5 items, got %d", len(s.List()))
		}
	})
}

func TestSandboxStore_Get(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		s := NewSandboxStore()
		sb := newTestSandbox("sb1", "poder1", StateRunning)
		_ = s.Add(sb)

		got, ok := s.Get("sb1")
		if !ok {
			t.Fatal("expected to find sb1, got not found")
		}
		if got.Name != "sb1" {
			t.Errorf("expected name sb1, got %s", got.Name)
		}
		if got.State != StateRunning {
			t.Errorf("expected state RUNNING, got %s", got.State)
		}
	})

	t.Run("not found", func(t *testing.T) {
		s := NewSandboxStore()
		_, ok := s.Get("nonexistent")
		if ok {
			t.Fatal("expected not found, got found")
		}
	})
}

func TestSandboxStore_Update(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		s := NewSandboxStore()
		sb := newTestSandbox("sb1", "poder1", StatePending)
		_ = s.Add(sb)

		err := s.Update("sb1", func(info *SandboxInfo) {
			info.State = StateRunning
			info.IP = "192.168.1.1"
		})
		if err != nil {
			t.Fatalf("Update failed: %v", err)
		}

		got, ok := s.Get("sb1")
		if !ok {
			t.Fatal("sb1 not found after update")
		}
		if got.State != StateRunning {
			t.Errorf("expected RUNNING state, got %s", got.State)
		}
		if got.IP != "192.168.1.1" {
			t.Errorf("expected IP 192.168.1.1, got %s", got.IP)
		}
	})

	t.Run("missing returns error", func(t *testing.T) {
		s := NewSandboxStore()
		err := s.Update("nonexistent", func(info *SandboxInfo) {
			info.State = StateRunning
		})
		if err == nil {
			t.Fatal("expected error for missing sandbox, got nil")
		}
	})
}

func TestSandboxStore_List(t *testing.T) {
	t.Run("empty store returns empty slice", func(t *testing.T) {
		s := NewSandboxStore()
		items := s.List()
		if items == nil {
			t.Fatal("List returned nil, expected empty slice")
		}
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})

	t.Run("returns all items", func(t *testing.T) {
		s := NewSandboxStore()
		names := []string{"alpha", "beta", "gamma"}
		for _, n := range names {
			_ = s.Add(newTestSandbox(n, "poder1", StatePending))
		}

		items := s.List()
		if len(items) != len(names) {
			t.Errorf("expected %d items, got %d", len(names), len(items))
		}

		// Verify all names present
		found := make(map[string]bool)
		for _, item := range items {
			found[item.Name] = true
		}
		for _, n := range names {
			if !found[n] {
				t.Errorf("missing sandbox %s in List result", n)
			}
		}
	})
}

func TestSandboxStore_ListByPoderID(t *testing.T) {
	t.Run("filters correctly", func(t *testing.T) {
		s := NewSandboxStore()
		_ = s.Add(newTestSandbox("sb1", "poder-A", StateRunning))
		_ = s.Add(newTestSandbox("sb2", "poder-A", StateRunning))
		_ = s.Add(newTestSandbox("sb3", "poder-B", StateRunning))
		_ = s.Add(newTestSandbox("sb4", "poder-B", StateStopped))

		listA := s.ListByPoderID("poder-A")
		if len(listA) != 2 {
			t.Errorf("expected 2 sandboxes for poder-A, got %d", len(listA))
		}
		for _, sb := range listA {
			if sb.PoderID != "poder-A" {
				t.Errorf("unexpected PoderID %s in poder-A list", sb.PoderID)
			}
		}

		listB := s.ListByPoderID("poder-B")
		if len(listB) != 2 {
			t.Errorf("expected 2 sandboxes for poder-B, got %d", len(listB))
		}
	})

	t.Run("returns empty for unknown poder", func(t *testing.T) {
		s := NewSandboxStore()
		_ = s.Add(newTestSandbox("sb1", "poder-A", StateRunning))

		list := s.ListByPoderID("poder-unknown")
		if len(list) != 0 {
			t.Errorf("expected 0 items for unknown poder, got %d", len(list))
		}
	})

	t.Run("empty store returns empty slice", func(t *testing.T) {
		s := NewSandboxStore()
		list := s.ListByPoderID("poder-A")
		if len(list) != 0 {
			t.Errorf("expected empty, got %d", len(list))
		}
	})
}

func TestSandboxStore_Delete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		s := NewSandboxStore()
		sb := newTestSandbox("sb1", "poder1", StateRunning)
		_ = s.Add(sb)

		if err := s.Delete("sb1"); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		_, ok := s.Get("sb1")
		if ok {
			t.Fatal("expected sb1 to be deleted, still found")
		}
		if len(s.List()) != 0 {
			t.Errorf("expected empty store after delete, got %d items", len(s.List()))
		}
	})

	t.Run("missing returns error", func(t *testing.T) {
		s := NewSandboxStore()
		err := s.Delete("nonexistent")
		if err == nil {
			t.Fatal("expected error for deleting nonexistent sandbox, got nil")
		}
	})

	t.Run("delete does not affect other items", func(t *testing.T) {
		s := NewSandboxStore()
		_ = s.Add(newTestSandbox("sb1", "poder1", StateRunning))
		_ = s.Add(newTestSandbox("sb2", "poder1", StateRunning))
		_ = s.Delete("sb1")

		if len(s.List()) != 1 {
			t.Errorf("expected 1 item after deleting sb1, got %d", len(s.List()))
		}
		_, ok := s.Get("sb2")
		if !ok {
			t.Fatal("sb2 should still exist after deleting sb1")
		}
	})
}

func TestSandboxStore_Concurrency(t *testing.T) {
	s := NewSandboxStore()

	const goroutines = 50
	var wg sync.WaitGroup

	// Concurrent adds
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			sb := newTestSandbox(fmt.Sprintf("sb-concurrent-%d", i), "poder1", StatePending)
			// Ignore duplicate errors — race adds expected
			_ = s.Add(sb)
		}()
	}
	wg.Wait()

	// Concurrent reads while writing
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("sb-concurrent-%d", i%goroutines)
			s.Get(name)
			s.List()
			s.ListByPoderID("poder1")
		}()
	}
	wg.Wait()

	// Concurrent updates
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("sb-concurrent-%d", i)
			_ = s.Update(name, func(info *SandboxInfo) {
				info.State = StateRunning
			})
		}()
	}
	wg.Wait()
}
