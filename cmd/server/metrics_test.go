package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

func TestMetricsHandler(t *testing.T) {
	stores := store.NewMemoryStores()
	_ = stores.Sandboxes.Add(&podpkg.SandboxInfo{Name: "a", State: podpkg.StateRunning, CreatedAt: time.Now()})
	_ = stores.Sandboxes.Add(&podpkg.SandboxInfo{Name: "b", State: podpkg.StateRunning, CreatedAt: time.Now()})
	_ = stores.Sandboxes.Add(&podpkg.SandboxInfo{Name: "c", State: podpkg.StateError, CreatedAt: time.Now()})
	stores.Poders.Register(&podpkg.RegisterPoderRequest{
		ID: "p1", Resources: podpkg.PoderResources{MaxContainers: 5},
	})
	stores.Poders.UpdateUsage("p1", func(u *podpkg.PoderUsage) { u.Containers = 2 })

	rec := httptest.NewRecorder()
	metricsHandler(&stores)(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`sandrpod_sandboxes{state="RUNNING"} 2`,
		`sandrpod_sandboxes{state="ERROR"} 1`,
		`sandrpod_poder_container_capacity 5`,
		`sandrpod_poder_containers_in_use 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, body)
		}
	}
}
