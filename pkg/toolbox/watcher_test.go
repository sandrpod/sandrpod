// Copyright 2024 SandrPod
package toolbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchManagerCreateWriteEvents(t *testing.T) {
	dir := t.TempDir()
	m := NewWatchManager()
	id, err := m.Create(dir, false)
	if err != nil {
		t.Fatalf("create watcher: %v", err)
	}
	defer m.Remove(id)

	// Create then modify a file inside the watched directory.
	f := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(f, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("bb"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Poll for events (fsnotify delivery is asynchronous).
	var types []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		evs, ok := m.Events(id)
		if !ok {
			t.Fatal("watcher vanished")
		}
		for _, ev := range evs {
			if ev.Name == "note.txt" {
				types = append(types, ev.Type)
			}
		}
		if len(types) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(types) == 0 {
		t.Fatal("expected at least one create/write event for note.txt")
	}
	// A drained watcher returns no events on the next poll.
	if evs, _ := m.Events(id); len(evs) != 0 {
		t.Fatalf("expected drained watcher to return 0 events, got %d", len(evs))
	}
}

func TestWatchManagerRemoveUnknown(t *testing.T) {
	m := NewWatchManager()
	if m.Remove("nope") {
		t.Fatal("removing an unknown watcher should return false")
	}
	if _, ok := m.Events("nope"); ok {
		t.Fatal("events for an unknown watcher should return ok=false")
	}
}

func TestCollectMetricsCPUCount(t *testing.T) {
	m := CollectMetrics()
	if m.CPUCount < 1 {
		t.Fatalf("CPUCount = %d, want >= 1", m.CPUCount)
	}
}
