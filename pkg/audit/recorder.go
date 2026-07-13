// Copyright 2026 SandrPod
// Local audit log writer.
//
// Format: NDJSON (one Event per line). Pros: trivial to parse, append-only,
// `tail -f` friendly, agnostic of any database. Cons: no random access.
// We don't need random access — uploader scans forward by byte offset.
//
// Rotation: when the active file exceeds `maxBytes` we rename to
// `audit-YYYYMMDD-HHMMSS.log` and start fresh. The uploader watches the
// directory, not a single inode, so rotation is transparent.
//
// Concurrency: a single Recorder serializes all writes through a mutex.
// Manager.Check / CheckExec / CheckPTY all push through the same instance
// installed in the agent's main loop. We deliberately do NOT use a channel
// + worker pattern: a slow disk should backpressure permission decisions
// (which are user-initiated and rare relative to OS work) rather than fill
// an unbounded queue.

package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sandrpod/sandrpod/pkg/homedir"
)

// DefaultMaxBytes triggers rotation when the active log exceeds 8 MiB.
// At ~150 bytes/event that's ~55k events — plenty of headroom for a single
// upload window and small enough to copy/parse without strain.
const DefaultMaxBytes int64 = 8 << 20

// Recorder appends Events to a rotating NDJSON file.
type Recorder struct {
	dir      string
	maxBytes int64

	mu   sync.Mutex
	file *os.File
	w    *bufio.Writer
	size int64
}

// Options configures a Recorder.
type Options struct {
	// Dir is the directory to hold audit logs. Created with 0700 if absent.
	Dir string
	// MaxBytes is the active-file rotation threshold (default DefaultMaxBytes).
	MaxBytes int64
}

// DefaultDir returns $HOME/.sandrpod/audit, the canonical location.
func DefaultDir() (string, error) {
	return filepath.Join(homedir.DataDir(), "audit"), nil
}

// NewRecorder opens (or creates) the active log file and returns a writer.
// The caller is responsible for calling Close() on shutdown.
func NewRecorder(opts Options) (*Recorder, error) {
	if opts.Dir == "" {
		def, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		opts.Dir = def
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(opts.Dir, 0700); err != nil {
		return nil, fmt.Errorf("create audit dir: %w", err)
	}
	r := &Recorder{dir: opts.Dir, maxBytes: opts.MaxBytes}
	if err := r.openActive(); err != nil {
		return nil, err
	}
	return r, nil
}

// activePath is the well-known name the active log lives at. The uploader
// looks for both this name AND any rotated `audit-*.log` siblings.
func (r *Recorder) activePath() string {
	return filepath.Join(r.dir, "active.log")
}

func (r *Recorder) openActive() error {
	f, err := os.OpenFile(r.activePath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat audit log: %w", err)
	}
	r.file = f
	r.w = bufio.NewWriterSize(f, 64<<10)
	r.size = stat.Size()
	return nil
}

// Record persists one event. Errors here mean the agent has lost
// observability — surface them to the caller, never swallow.
func (r *Recorder) Record(ev Event) error {
	if ev.EventID == "" {
		ev.EventID = newEventID()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Rotation check happens BEFORE write so the active file never crosses
	// maxBytes by more than one event's worth.
	if r.size >= r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return err
		}
	}

	n, err := r.w.Write(data)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	if err := r.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	// Flush per record. NDJSON readers (uploader, `tail -f`) need to see
	// each line promptly; the bufio.Writer is sized large enough that the
	// flush is essentially a memcpy + write syscall.
	if err := r.w.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	r.size += int64(n + 1)
	return nil
}

// rotateLocked closes the active file, renames it with a timestamp suffix,
// and opens a fresh active file. Caller must hold r.mu.
func (r *Recorder) rotateLocked() error {
	if err := r.w.Flush(); err != nil {
		return fmt.Errorf("flush before rotate: %w", err)
	}
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("close before rotate: %w", err)
	}
	rotated := filepath.Join(r.dir, fmt.Sprintf("audit-%s.log", time.Now().UTC().Format("20060102-150405")))
	if err := os.Rename(r.activePath(), rotated); err != nil {
		return fmt.Errorf("rotate: %w", err)
	}
	return r.openActive()
}

// Close flushes and closes the active file. Safe to call once.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w != nil {
		_ = r.w.Flush()
	}
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// Dir returns the audit directory path (used by uploader for discovery).
func (r *Recorder) Dir() string { return r.dir }

// newEventID returns a 16-byte hex string. We avoid pulling in google/uuid
// to keep the audit package dependency-free; the underlying entropy source
// (crypto/rand wrapped through a small helper) gives us collision-resistant
// IDs for event volumes orders of magnitude beyond what we expect.
func newEventID() string {
	const hex = "0123456789abcdef"
	var b [32]byte
	for i := 0; i < 16; i++ {
		x := randByte()
		b[2*i] = hex[x>>4]
		b[2*i+1] = hex[x&0x0f]
	}
	return string(b[:])
}
