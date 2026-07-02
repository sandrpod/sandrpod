package audit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Regression: when the first POST fails, the batch must be re-sent on the next
// cycle — never silently skipped by an in-memory cursor advanced ahead of a
// successful delivery. Contract: at-least-once (no loss; dupes tolerated).
func TestUploader_RetryDoesNotDropBatch(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(Options{Dir: dir})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	defer rec.Close()

	var (
		mu       sync.Mutex
		gotPaths []string
		failNext = true // fail the first POST, then accept
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if failNext {
			failNext = false
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var b Batch
		if err := json.Unmarshal(body, &b); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		for _, e := range b.Events {
			gotPaths = append(gotPaths, e.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	for i := range 3 {
		_ = rec.Record(Event{Source: SourcePathCheck, Decision: "allow", Path: "/p" + string(rune('A'+i))})
	}

	up, err := NewUploader(UploaderOptions{
		URL:        srv.URL,
		Token:      "t",
		Recorder:   rec,
		Interval:   20 * time.Millisecond,
		MaxBackoff: 40 * time.Millisecond,
		BatchSize:  10,
	})
	if err != nil || up == nil {
		t.Fatalf("NewUploader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	up.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(gotPaths)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	up.Stop()

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, p := range gotPaths {
		seen[p] = true
	}
	for _, want := range []string{"/pA", "/pB", "/pC"} {
		if !seen[want] {
			t.Errorf("event %q was DROPPED after the POST retry (got %v)", want, gotPaths)
		}
	}
}
