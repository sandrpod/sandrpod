package server

import (
	"sync"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

type recordingSink struct {
	mu  sync.Mutex
	got []string
}

func (r *recordingSink) Emit(event string, data map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, event+":"+data["from"].(string))
}

// A metering sink bills by these events. Before them it saw only create and
// delete, so a stopped, paused or idle-reaped sandbox was billed until deleted.
func TestTransitionEvent(t *testing.T) {
	cases := []struct {
		from, to podpkg.State
		want     string
	}{
		{podpkg.StateRunning, podpkg.StateStopped, "sandbox.stopped"},
		{podpkg.StateStopping, podpkg.StateStopped, "sandbox.stopped"},
		{podpkg.StateRunning, podpkg.StateError, "sandbox.stopped"}, // container vanished
		{podpkg.StateStopped, podpkg.StateRunning, "sandbox.started"},
		{podpkg.StateError, podpkg.StateRunning, "sandbox.started"}, // reconcile restore
		{podpkg.StateRunning, podpkg.StateRunning, ""},
		{podpkg.StateStopped, podpkg.StateStopped, ""},
		{podpkg.StatePending, podpkg.StateRunning, ""},  // creation has sandbox.running
		{podpkg.StateStarting, podpkg.StateRunning, ""}, // same
		{podpkg.StatePending, podpkg.StateError, ""},    // never ran
		{podpkg.StateStopped, podpkg.StateError, ""},    // was not running
	}
	for _, c := range cases {
		if got := transitionEvent(c.from, c.to); got != c.want {
			t.Errorf("%s → %s = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

func TestTransitionStateEmitsOnce(t *testing.T) {
	prev := events
	sink := &recordingSink{}
	events = sink
	t.Cleanup(func() { events = prev })

	ss := store.NewMemoryStores().Sandboxes
	if err := ss.Add(&podpkg.SandboxInfo{Name: "sb-1", Owner: "w", State: podpkg.StateRunning}); err != nil {
		t.Fatal(err)
	}
	transitionState(ss, "sb-1", podpkg.StateStopped)
	transitionState(ss, "sb-1", podpkg.StateStopped) // already stopped: no second event
	transitionState(ss, "sb-1", podpkg.StateRunning)
	transitionState(ss, "missing", podpkg.StateRunning) // unknown record: nothing

	want := []string{"sandbox.stopped:RUNNING", "sandbox.started:STOPPED"}
	if len(sink.got) != len(want) {
		t.Fatalf("events = %v, want %v", sink.got, want)
	}
	for i := range want {
		if sink.got[i] != want[i] {
			t.Fatalf("events = %v, want %v", sink.got, want)
		}
	}
	if sb, _ := ss.Get("sb-1"); sb.State != podpkg.StateRunning {
		t.Fatalf("state = %s, want RUNNING", sb.State)
	}
}
