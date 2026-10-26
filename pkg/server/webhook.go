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
		"name":       sb.Name,
		"provider":   sb.ProviderType,
		"state":      string(sb.State),
		"owner":      sb.Owner,
		"region":     sb.Region,
		"cpu_cores":  sb.CPUCores,
		"memory_mb":  sb.MemoryMB,
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
