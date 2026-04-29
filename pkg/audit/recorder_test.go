// Copyright 2026 SandrPod
// Tests for recorder + uploader round-trip.

package audit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRecorder_AppendsAndAssignsIDs(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(Options{Dir: dir})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	defer rec.Close()

	for i := 0; i < 3; i++ {
		if err := rec.Record(Event{Source: SourcePathCheck, Decision: "allow"}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// Read back.
	data, err := readActive(dir)
	if err != nil {
		t.Fatalf("readActive: %v", err)
	}
	lines := splitNDJSON(data)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	for _, line := range lines {
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if ev.EventID == "" {
			t.Error("event_id should be auto-assigned")
		}
		if ev.OccurredAt.IsZero() {
			t.Error("occurred_at should be auto-assigned")
		}
	}
}

func TestUploader_PostsBatchAndAdvancesCursor(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(Options{Dir: dir})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	defer rec.Close()

	// Spy server that captures bodies and returns 204.
	var (
		mu        sync.Mutex
		received  []Batch
		authToken string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		authToken = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var b Batch
		if err := json.Unmarshal(body, &b); err != nil {
			http.Error(w, "bad json: "+err.Error(), 400)
			return
		}
		received = append(received, b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	for i := 0; i < 5; i++ {
		_ = rec.Record(Event{Source: SourcePathCheck, Decision: "allow", Path: "/p"})
	}

	up, err := NewUploader(UploaderOptions{
		URL:          srv.URL,
		Token:        "test-token",
		Recorder:     rec,
		Interval:     20 * time.Millisecond,
		BatchSize:    10,
		AgentVersion: "test-1.0",
		SandboxName:  "test-sandbox",
		HostOS:       "darwin",
		HostArch:     "arm64",
	})
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}
	if up == nil {
		t.Fatal("uploader should not be nil when URL+token set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	up.Start(ctx)

	// Wait until the spy has received our batch.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	up.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatal("server received no batches")
	}
	if authToken != "Bearer test-token" {
		t.Errorf("auth header: got %q, want Bearer test-token", authToken)
	}
	totalEvents := 0
	for _, b := range received {
		totalEvents += len(b.Events)
		if b.Version != CurrentBatchVersion {
			t.Errorf("batch version: got %d, want %d", b.Version, CurrentBatchVersion)
		}
		for _, ev := range b.Events {
			// Uploader enrichment must have populated identity fields.
			if ev.AgentVersion != "test-1.0" || ev.SandboxName != "test-sandbox" {
				t.Errorf("identity fields not enriched: %+v", ev)
			}
		}
	}
	if totalEvents != 5 {
		t.Errorf("uploaded %d events, want 5", totalEvents)
	}

	// Cursor must have advanced.
	cursorPath := filepath.Join(dir, "audit.cursor")
	data, err := readFile(cursorPath)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if len(data) == 0 {
		t.Error("cursor file should not be empty after upload")
	}
}

// readActive returns the contents of active.log (helper for tests).
func readActive(dir string) ([]byte, error) {
	return readFile(filepath.Join(dir, "active.log"))
}

func readFile(path string) ([]byte, error) {
	return io.ReadAll(mustOpen(path))
}

func mustOpen(path string) io.ReadCloser {
	f, err := openFile(path)
	if err != nil {
		panic(err)
	}
	return f
}

// splitNDJSON splits raw bytes on \n, dropping the final empty line.
func splitNDJSON(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			out = append(out, data[start:i])
			start = i + 1
		}
	}
	return out
}
