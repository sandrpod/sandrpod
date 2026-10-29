// Copyright 2026 SandrPod Contributors
// Worker credentials: the invitation becomes a key, once.
//
// The machine starts with a join token — short-lived, pasted from a console, meant
// to be used once. On the first connection it exchanges that for a worker key bound
// to its poder id and writes it beside its identity. From then on the key is what it
// authenticates with, so a reboot a week later still works and the invitation can be
// allowed to expire in an hour.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// workerKeyFile is where the credential lives: next to the poder id, in the volume
// that survives a container restart, readable only by the worker.
const workerKeyFile = "worker-key"

// workerKey is what is persisted: the credential and when it stops working.
type workerKey struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// resolveWorkerToken decides what this worker authenticates with.
//
// Priority, and why: a stored key beats the command line, because the key is the
// long-lived credential and the command line keeps whatever was pasted the first
// time. A `wk_` token on the command line is used as-is. A `jn_` token is an
// invitation and is exchanged. Anything else (a platform token, a self-hosted static
// token) is passed through untouched.
func resolveWorkerToken(apiURL, presented, poderID string) string {
	dataDir := env("PODER_DATA_DIR", "/var/lib/sandrpod")
	path := filepath.Join(dataDir, workerKeyFile)

	if key, ok := readWorkerKey(path); ok {
		// Expiry is checked here rather than left to the server: a worker that
		// knows its key is dead can say so in its own log, where the operator is
		// looking, instead of reporting "bad handshake" forever.
		if exp, err := time.Parse(time.RFC3339, key.ExpiresAt); err != nil || time.Now().Before(exp) {
			if presented != "" && strings.HasPrefix(presented, "jn_") {
				log.Printf("worker key (expires %s) takes precedence over the join token on the command line", key.ExpiresAt)
			} else {
				log.Printf("Using the worker key from %s (expires %s)", path, key.ExpiresAt)
			}
			return key.Token
		}
		log.Printf("Worker key in %s expired on %s; re-enrolling", path, key.ExpiresAt)
	}

	if !strings.HasPrefix(presented, "jn_") {
		return presented
	}
	if apiURL == "" {
		log.Printf("Join token given but no API URL to enrol against; using it as-is")
		return presented
	}

	key, err := enrol(apiURL, presented, poderID)
	if err != nil {
		log.Printf("Enrolment failed: %v", err)
		log.Printf("  the join token is an invitation: it expires within an hour and is " +
			"exchanged for a worker key on first use.")
		log.Printf("  mint a new one in the console (节点 → 添加节点) and restart this worker " +
			"with it; the running worker keeps retrying either way.")
		// Fall back to the invitation: a control plane that predates the enrolment
		// endpoint still accepts join tokens directly, and refusing to start would
		// turn a missing feature into an outage.
		return presented
	}
	if err := writeWorkerKey(path, key); err != nil {
		log.Printf("WARNING: could not persist the worker key to %s (%v); it will not survive a restart", path, err)
	} else {
		log.Printf("Enrolled as worker %s — credential stored in %s, expires %s", poderID, path, key.ExpiresAt)
	}
	return key.Token
}

func readWorkerKey(path string) (workerKey, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return workerKey{}, false
	}
	var k workerKey
	if err := json.Unmarshal(data, &k); err != nil || k.Token == "" {
		return workerKey{}, false
	}
	return k, true
}

func writeWorkerKey(path string, k workerKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(k)
	if err != nil {
		return err
	}
	// 0600 and an atomic rename: a half-written key file is a worker that cannot
	// authenticate after the next restart.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// enrol posts the invitation and returns the credential it buys.
func enrol(apiURL, joinToken, poderID string) (workerKey, error) {
	body, err := json.Marshal(map[string]string{"poder_id": poderID})
	if err != nil {
		return workerKey{}, err
	}
	url := strings.TrimSuffix(apiURL, "/") + "/api/v1/poders/enroll"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return workerKey{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+joinToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return workerKey{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return workerKey{}, fmt.Errorf("control plane answered %s (an invitation is valid for "+
			"an hour and is exchanged once; mint a new one)", resp.Status)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return workerKey{}, err
	}
	if out.Token == "" {
		return workerKey{}, fmt.Errorf("control plane returned no credential")
	}
	return workerKey{Token: out.Token, ExpiresAt: out.ExpiresAt}, nil
}
