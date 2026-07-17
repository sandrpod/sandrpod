// Copyright 2026 SandrPod Contributors
// Filesystem watcher backing the E2B watch_dir surface. The SDK model is
// poll-based: create a watcher for a directory, then poll for the events that
// have accrued since the last poll, then remove it. We keep an fsnotify watcher
// per watcher-id with a buffer a poll drains.

package toolbox

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// randHex returns n random bytes hex-encoded (2n chars).
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// WatchEvent is one filesystem change. Type is create|write|remove|rename|chmod
// (the E2B EventType names, lowercased); Name is the affected entry's basename.
type WatchEvent struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type watcher struct {
	fsw *fsnotify.Watcher
	mu  sync.Mutex
	buf []WatchEvent
	dir string
}

func (w *watcher) append(ev WatchEvent) {
	w.mu.Lock()
	w.buf = append(w.buf, ev)
	w.mu.Unlock()
}

// drain returns the events accrued since the last drain and clears the buffer.
func (w *watcher) drain() []WatchEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.buf
	w.buf = nil
	if out == nil {
		out = []WatchEvent{}
	}
	return out
}

// WatchManager owns the active watchers keyed by id.
type WatchManager struct {
	mu       sync.Mutex
	watchers map[string]*watcher
}

// NewWatchManager builds an empty manager.
func NewWatchManager() *WatchManager {
	return &WatchManager{watchers: map[string]*watcher{}}
}

// Create starts watching dir and returns a watcher id. recursive also watches
// existing subdirectories (subdirectories created later are best-effort: a new
// directory event adds it to the watch).
func (m *WatchManager) Create(dir string, recursive bool) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", &os.PathError{Op: "watch", Path: dir, Err: os.ErrInvalid}
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return "", err
	}
	if err := fsw.Add(dir); err != nil {
		fsw.Close()
		return "", err
	}
	w := &watcher{fsw: fsw, dir: dir}
	if recursive {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && p != dir {
				_ = fsw.Add(p)
			}
			return nil
		})
	}
	id := randHex(16)
	m.mu.Lock()
	m.watchers[id] = w
	m.mu.Unlock()

	go func() {
		for {
			select {
			case ev, ok := <-fsw.Events:
				if !ok {
					return
				}
				t := mapWatchOp(ev.Op)
				if t == "" {
					continue
				}
				// Keep recursive watches following newly-created subdirectories.
				if recursive && ev.Op&fsnotify.Create != 0 {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
						_ = fsw.Add(ev.Name)
					}
				}
				w.append(WatchEvent{Name: filepath.Base(ev.Name), Type: t})
			case _, ok := <-fsw.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return id, nil
}

// Events drains the buffered events for a watcher. ok is false if unknown.
func (m *WatchManager) Events(id string) ([]WatchEvent, bool) {
	m.mu.Lock()
	w, ok := m.watchers[id]
	m.mu.Unlock()
	if !ok {
		return nil, false
	}
	return w.drain(), true
}

// Remove stops and drops a watcher.
func (m *WatchManager) Remove(id string) bool {
	m.mu.Lock()
	w, ok := m.watchers[id]
	delete(m.watchers, id)
	m.mu.Unlock()
	if !ok {
		return false
	}
	_ = w.fsw.Close()
	return true
}

// mapWatchOp maps an fsnotify op to the E2B event-type name (empty = ignore).
func mapWatchOp(op fsnotify.Op) string {
	switch {
	case op&fsnotify.Create != 0:
		return "create"
	case op&fsnotify.Write != 0:
		return "write"
	case op&fsnotify.Remove != 0:
		return "remove"
	case op&fsnotify.Rename != 0:
		return "rename"
	case op&fsnotify.Chmod != 0:
		return "chmod"
	}
	return ""
}
