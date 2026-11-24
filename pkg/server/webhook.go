// Copyright 2026 SandrPod Contributors
// Lifecycle events: an interface plus the built-in webhook implementation.
//
// The interface exists so a deployment can route the same events somewhere else
// — a metering pipeline, an audit exporter, an alerting system — without the
// control plane growing a branch per destination. Events are fire-and-forget:
// a slow or broken sink must never hold up a sandbox.

package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// EventSink receives lifecycle notifications.
//
// Emit is called from request and reaper paths, so an implementation must not
// block: do the work in a goroutine (the webhook sink does) or drop the event.
// It must also be safe for concurrent use.
type EventSink interface {
	Emit(event string, data map[string]any)
}

// webhookURL is the operator-configured event sink (empty = disabled).
var webhookURL = os.Getenv("SANDRPOD_WEBHOOK_URL")

var webhookClient = &http.Client{Timeout: 5 * time.Second}

// events is the process-wide sink. New() replaces it when Config.Events is set
// — the same "last constructor wins" rule the rest of this package follows, and
// the reason a program builds exactly one mux.
var events EventSink = webhookSink{}

// webhookSink POSTs {event, time, data} to SANDRPOD_WEBHOOK_URL.
type webhookSink struct{}

func (webhookSink) Emit(event string, data map[string]any) {
	if webhookURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"event": event,
		"time":  time.Now().UTC().Format(time.RFC3339),
		"data":  data,
	})
	go func() {
		resp, err := webhookClient.Post(webhookURL, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("webhook %s: %v", event, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("webhook %s: sink returned %d", event, resp.StatusCode)
		}
	}()
}

// notifyEvent emits through the configured sink. Failures are the sink's
// problem: they are logged there and never affect the calling flow.
func notifyEvent(event string, data map[string]any) {
	if events == nil {
		return
	}
	events.Emit(event, data)
}

// sandboxEvent builds the payload for a sandbox lifecycle event: who owns it,
// what it reserved, and where it runs. A metering or audit sink needs all
// three, and reconstructing them downstream from a name is not possible —
// resources are what the caller asked for, not a property of the name.
func sandboxEvent(sb *podpkg.SandboxInfo, extra map[string]any) map[string]any {
	out := map[string]any{
		"name":       sb.DisplayNameOf(),
		"routing_id": sb.Name,
		"provider":   sb.ProviderType,
		"state":      string(sb.State),
		"owner":      sb.Owner,
		"region":     sb.Region,
		"cpu_cores":  sb.CPUCores,
		"memory_mb":  sb.MemoryMB,
		// The runtime is part of the security posture the lifecycle event is
		// reporting; a gVisor sandbox should not be indistinguishable from a
		// runc one in metering/audit consumers.
		"runtime": sb.Runtime,
		// gpu rides along for metering: a GPU sandbox costs an order of magnitude
		// more than the vCPU numbers suggest.
		"gpu":        sb.GPU,
		"poder_id":   sb.PoderID,
		"created_at": sb.CreatedAt.UTC().Format(time.RFC3339),
	}
	if sb.TTLSeconds > 0 {
		out["ttl_seconds"] = sb.TTLSeconds
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// stateStore is what a lifecycle transition needs from the sandbox store.
type stateStore interface {
	Get(name string) (*podpkg.SandboxInfo, bool)
	Update(name string, fn func(*podpkg.SandboxInfo)) error
}

// transitionState moves a sandbox to a new state and, when that begins or ends a
// stretch of running, reports it to the event sink (see transitionEvent).
func transitionState(ss stateStore, name string, to podpkg.State) {
	var from podpkg.State
	if err := ss.Update(name, func(s *podpkg.SandboxInfo) {
		from = s.State
		s.State = to
	}); err != nil {
		return
	}
	emitTransition(ss, name, from, to)
}

// emitTransition reports a state change that already happened.
func emitTransition(ss stateStore, name string, from, to podpkg.State) {
	ev := transitionEvent(from, to)
	if ev == "" {
		return
	}
	if sb, ok := ss.Get(name); ok {
		notifyEvent(ev, sandboxEvent(sb, map[string]any{"from": string(from)}))
	}
}

// transitionEvent names the lifecycle event for a state change, or "" when
// nothing a consumer bills by changed.
//
//	sandbox.stopped  a running sandbox stopped: by request, by an E2B pause, by
//	                 the idle reaper, or because its container disappeared
//	sandbox.started  a stopped (or errored) sandbox runs again
//
// Creation keeps its own sandbox.running and deletion its sandbox.deleted.
// Without these two, a metering sink saw only create and delete, and billed a
// stopped or paused sandbox for every hour until it was deleted.
func transitionEvent(from, to podpkg.State) string {
	if from == to {
		return ""
	}
	switch to {
	case podpkg.StateStopped, podpkg.StateError:
		if from == podpkg.StateRunning || from == podpkg.StateStopping {
			return "sandbox.stopped"
		}
	case podpkg.StateRunning:
		if from == podpkg.StateStopped || from == podpkg.StateStopping || from == podpkg.StateError {
			return "sandbox.started"
		}
	}
	return ""
}
