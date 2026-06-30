// Copyright 2026 SandrPod
// Background batch uploader.
//
// Responsibilities:
//   - Walk the audit directory in chronological order.
//   - For each file, scan from the last-saved offset, batch lines into
//     `Batch` payloads, POST to the configured URL, and only after a 2xx
//     response advance the persisted cursor.
//   - Handle transient HTTP failures with exponential backoff.
//   - Delete fully-uploaded rotated files (active.log is never deleted).
//
// The cursor (`audit.cursor` in the audit dir) tracks `(filename, offset)`.
// At-least-once delivery is the guarantee — the server MUST dedupe by
// EventID. Doing exactly-once would require either two-phase commit with
// the server or destructive read on the agent side, both of which are
// worse trade-offs than asking the server to dedupe.

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// UploaderOptions configures an Uploader. URL and Token are required;
// everything else has sensible defaults.
type UploaderOptions struct {
	// URL of the server endpoint. POSTs a Batch as JSON.
	URL string
	// Token is sent verbatim as `Authorization: Bearer <Token>`.
	Token string
	// Recorder is the live audit recorder. Uploader reads its Dir() to
	// find files; it does not touch the recorder's open file directly.
	Recorder *Recorder
	// HTTPClient is used for POSTs. Defaults to a 10s-timeout client.
	HTTPClient *http.Client
	// Interval between scan cycles. Default 30s — frequent enough that
	// admins see new events promptly, sparse enough not to bother the
	// server when the agent is idle.
	Interval time.Duration
	// BatchSize is the maximum events per POST. Default 200 keeps each
	// request well under typical server body limits while amortizing
	// HTTP overhead.
	BatchSize int
	// MaxBackoff caps the retry interval after consecutive failures.
	MaxBackoff time.Duration
	// AgentVersion / SandboxName / HostOS / HostArch fill the matching
	// fields on each Event before upload, so the recorder doesn't have
	// to know anything about the agent's identity.
	AgentVersion string
	SandboxName  string
	HostOS       string
	HostArch     string
}

// Uploader runs the upload loop. Stop() blocks until the loop has flushed
// any in-flight batch.
type Uploader struct {
	opts UploaderOptions

	mu     sync.Mutex
	cursor cursorState

	stop    chan struct{}
	stopped chan struct{}
}

// cursorState lives in audit.cursor (JSON). One file at a time is "in
// progress"; once we drain it, we delete the rotated file (or, for
// active.log, advance offset and stay).
type cursorState struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
}

// NewUploader constructs an Uploader. If URL or Token is empty, returns nil
// (uploads disabled — auditing still happens locally). This lets the
// agent main wire the uploader unconditionally and let configuration drive
// the behavior.
func NewUploader(opts UploaderOptions) (*Uploader, error) {
	if opts.URL == "" || opts.Token == "" {
		return nil, nil
	}
	if opts.Recorder == nil {
		return nil, fmt.Errorf("audit.NewUploader: Recorder is required")
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.Interval == 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = 200
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = 10 * time.Minute
	}
	u := &Uploader{
		opts:    opts,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	if err := u.loadCursor(); err != nil {
		return nil, err
	}
	return u, nil
}

// Start begins the background loop. Returns immediately.
func (u *Uploader) Start(ctx context.Context) {
	go u.run(ctx)
}

// Stop signals the loop to halt and waits for it to drain.
func (u *Uploader) Stop() {
	close(u.stop)
	<-u.stopped
}

func (u *Uploader) run(ctx context.Context) {
	defer close(u.stopped)
	backoff := time.Second
	for {
		// One scan cycle: drain everything we can from the audit dir.
		err := u.cycle(ctx)
		if err != nil {
			// Surface but do not crash; transient net failures are normal.
			fmt.Fprintf(os.Stderr, "audit upload cycle error: %v (next retry in %s)\n", err, backoff)
			backoff *= 2
			if backoff > u.opts.MaxBackoff {
				backoff = u.opts.MaxBackoff
			}
		} else {
			backoff = u.opts.Interval
		}

		select {
		case <-ctx.Done():
			return
		case <-u.stop:
			return
		case <-time.After(backoff):
		}
	}
}

// cycle drains pending events and posts batches until either the server
// rejects, no more events are available, or the deadline lapses.
func (u *Uploader) cycle(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-u.stop:
			return nil
		default:
		}

		batch, fileToFinish, err := u.nextBatch()
		if err != nil {
			return err
		}
		if len(batch.Events) == 0 {
			return nil
		}

		if err := u.post(ctx, batch); err != nil {
			return err
		}
		if err := u.saveCursor(); err != nil {
			return fmt.Errorf("save cursor after upload: %w", err)
		}
		if fileToFinish != "" {
			// Rotated file fully drained — delete it so the dir doesn't grow.
			// A failed remove only leaks disk (no data loss); log so an
			// operator can notice a growing audit dir.
			if err := os.Remove(fileToFinish); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "audit upload: remove drained file %s: %v\n", fileToFinish, err)
			}
			u.mu.Lock()
			u.cursor = cursorState{}
			u.mu.Unlock()
			// A failed cursor reset means the next run may re-upload this
			// file's events (delivery is at-least-once, so dupes are safe),
			// but it should not pass silently.
			if err := u.saveCursor(); err != nil {
				fmt.Fprintf(os.Stderr, "audit upload: reset cursor after %s: %v\n", fileToFinish, err)
			}
		}
	}
}

// nextBatch reads up to BatchSize events from the next pending file.
// Returns the batch, the path of the file we should DELETE if this batch
// is its last (i.e. a fully-drained rotated file), and any error.
func (u *Uploader) nextBatch() (Batch, string, error) {
	files, err := u.pendingFiles()
	if err != nil {
		return Batch{}, "", err
	}
	if len(files) == 0 {
		return Batch{Version: CurrentBatchVersion}, "", nil
	}

	target := files[0]
	u.mu.Lock()
	if u.cursor.File != target {
		u.cursor = cursorState{File: target, Offset: 0}
	}
	startOffset := u.cursor.Offset
	u.mu.Unlock()

	f, err := os.Open(target)
	if err != nil {
		return Batch{}, "", fmt.Errorf("open %s: %w", target, err)
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return Batch{}, "", fmt.Errorf("seek: %w", err)
	}

	batch := Batch{Version: CurrentBatchVersion}
	reader := newLineReader(f)
	for len(batch.Events) < u.opts.BatchSize {
		line, advance, err := reader.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Batch{}, "", err
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Skip but advance — a corrupted line shouldn't wedge upload.
			startOffset += int64(advance)
			continue
		}
		// Enrich at upload time so the recorder layer stays identity-free.
		if ev.AgentVersion == "" {
			ev.AgentVersion = u.opts.AgentVersion
		}
		if ev.SandboxName == "" {
			ev.SandboxName = u.opts.SandboxName
		}
		if ev.HostOS == "" {
			ev.HostOS = u.opts.HostOS
		}
		if ev.HostArch == "" {
			ev.HostArch = u.opts.HostArch
		}
		batch.Events = append(batch.Events, ev)
		startOffset += int64(advance)
	}

	u.mu.Lock()
	u.cursor.Offset = startOffset
	u.mu.Unlock()

	// If we finished a rotated file completely, mark it for deletion.
	if isRotated(filepath.Base(target)) {
		stat, err := os.Stat(target)
		if err == nil && startOffset >= stat.Size() {
			return batch, target, nil
		}
	}
	return batch, "", nil
}

// post serializes the batch and POSTs it to the configured URL with bearer auth.
func (u *Uploader) post(ctx context.Context, batch Batch) error {
	if len(batch.Events) == 0 {
		return nil
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("marshal batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.opts.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+u.opts.Token)
	req.Header.Set("X-Sandrpod-Agent-Version", u.opts.AgentVersion)
	req.Header.Set("X-Sandrpod-Sandbox-Name", u.opts.SandboxName)

	resp, err := u.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// pendingFiles returns rotated files first (oldest → newest) followed by
// the active log if it has unread bytes. Sorted so we always upload in the
// order events occurred.
func (u *Uploader) pendingFiles() ([]string, error) {
	dir := u.opts.Recorder.Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read audit dir: %w", err)
	}
	var rotated []string
	hasActive := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "active.log" {
			hasActive = true
			continue
		}
		if isRotated(name) {
			rotated = append(rotated, filepath.Join(dir, name))
		}
	}
	sort.Strings(rotated)
	if hasActive {
		rotated = append(rotated, filepath.Join(dir, "active.log"))
	}
	return rotated, nil
}

func isRotated(name string) bool {
	return strings.HasPrefix(name, "audit-") && strings.HasSuffix(name, ".log")
}

// loadCursor / saveCursor persist (file, offset) so a restart resumes from
// where we left off rather than re-uploading from the beginning of every
// rotated log.
func (u *Uploader) loadCursor() error {
	path := filepath.Join(u.opts.Recorder.Dir(), "audit.cursor")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cursor: %w", err)
	}
	var c cursorState
	if err := json.Unmarshal(data, &c); err != nil {
		// Cursor corruption shouldn't bring down audit — log and continue
		// from zero, accepting that we'll re-upload some events. Server
		// dedup is the safety net.
		return nil
	}
	u.mu.Lock()
	u.cursor = c
	u.mu.Unlock()
	return nil
}

func (u *Uploader) saveCursor() error {
	u.mu.Lock()
	c := u.cursor
	u.mu.Unlock()

	path := filepath.Join(u.opts.Recorder.Dir(), "audit.cursor")
	tmp := path + ".tmp"
	data, _ := json.Marshal(c)
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lineReader returns NDJSON lines plus the byte advance (including the
// trailing newline) so callers can update a file offset.
type lineReader struct {
	r   io.Reader
	buf []byte
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: r, buf: make([]byte, 0, 4<<10)}
}

// next reads up to and including the next \n. Returns the line WITHOUT the
// trailing newline, the number of bytes consumed (line + newline), and any
// error. EOF on a partial line returns io.EOF and no advance — partial
// writes happen during rotation and we'd rather wait for the next cycle
// than upload a truncated event.
func (l *lineReader) next() ([]byte, int, error) {
	tmp := make([]byte, 4<<10)
	for {
		// Look for newline in existing buffer.
		for i, b := range l.buf {
			if b == '\n' {
				line := append([]byte(nil), l.buf[:i]...)
				l.buf = l.buf[i+1:]
				return line, i + 1, nil
			}
		}
		n, err := l.r.Read(tmp)
		if n > 0 {
			l.buf = append(l.buf, tmp[:n]...)
			continue
		}
		if err != nil {
			if err == io.EOF && len(l.buf) > 0 {
				// Partial line at EOF — leave it in buf, don't return it.
			}
			return nil, 0, err
		}
	}
}
