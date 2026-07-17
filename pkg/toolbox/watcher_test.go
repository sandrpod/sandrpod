// Copyright 2026 SandrPod Contributors
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
	// Draining: with no further FS activity the buffer must go — and stay —
	// empty. Late stragglers may still be in flight (Linux inotify delivers
	// create and write separately), so drain until two consecutive polls come
	// back empty instead of asserting emptiness immediately.
	empties := 0
	for time.Now().Before(deadline) && empties < 2 {
		evs, ok := m.Events(id)
		if !ok {
			t.Fatal("watcher vanished")
		}
		if len(evs) == 0 {
			empties++
		} else {
			empties = 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	if empties < 2 {
		t.Fatal("watcher buffer never drained to empty")
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
