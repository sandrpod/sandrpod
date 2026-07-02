// Copyright 2024 SandrPod
// Lifecycle webhooks: fire-and-forget POSTs to an operator-configured URL so
// external systems can react to sandbox/poder transitions without polling.

package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// webhookURL is the operator-configured event sink (empty = disabled).
var webhookURL = os.Getenv("SANDRPOD_WEBHOOK_URL")

var webhookClient = &http.Client{Timeout: 5 * time.Second}

// notifyEvent POSTs {event, time, data} to SANDRPOD_WEBHOOK_URL asynchronously.
// Failures are logged and never affect the calling flow.
func notifyEvent(event string, data map[string]any) {
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
