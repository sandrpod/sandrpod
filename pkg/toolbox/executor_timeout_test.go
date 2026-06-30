package toolbox

import (
	"context"
	"testing"
	"time"
)

// ExecuteStream's timeout path used to call cmd.Wait() twice (once in the
// waiter goroutine, once in the ctx.Done branch) and read the output buffers
// while io.Copy was still writing — both data races. This test drives the
// timeout path under -race and asserts the 124 result, locking the fix.
func TestExecuteStream_TimeoutIsRaceFree(t *testing.T) {
	requireCmd(t, "bash")

	e := NewExecutor()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var events int
	res, err := e.ExecuteStream(ctx, "bash", "sleep 5", func(string, string) { events++ })
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if res.ExitCode != 124 {
		t.Fatalf("expected timeout exit code 124, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
	if time.Since(res.StartedAt) > 4*time.Second {
		t.Fatalf("timeout did not fire promptly: ran %s", time.Since(res.StartedAt))
	}
}
