// Copyright 2026 SandrPod Contributors
// Agent credentials: the invitation becomes a key, once.
//
// This is the sandbox-side counterpart of cmd/poder/enrol.go. The machine starts
// with a join token read off the console — short-lived, meant to be pasted once —
// and exchanges it for a worker key bound to a stable id of its own. From then on
// the key is what it authenticates with, so a reboot an hour later still works.
//
// Without this the agent kept presenting the invitation at every reconnect. It
// worked until the invitation expired and then the sandbox disappeared from the
// fleet on the first network blip, with a 401 in a log nobody was watching.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// agentCredentialFile lives in the agent's state directory (default ~/.sandrpod),
// beside permissions, audit and mcp.json. The service runs as the invoking user,
// so this survives reboots without needing root-owned state.
const agentCredentialFile = "worker-key"

// agentCredential is what is persisted: the machine's stable id, the credential,
// and when it stops working.
type agentCredential struct {
	PoderID   string `json:"poder_id,omitempty"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// agentCurrentToken is the credential in use. It starts as whatever was resolved
// at startup and is replaced when a background enrolment succeeds.
var agentCurrentToken atomic.Value

// token is the credential every control-plane request carries. It falls back to
// the command-line token when nothing has been resolved yet, which keeps tests
// and self-hosted static-token deployments working unchanged.
func activeToken() string {
	if v, ok := agentCurrentToken.Load().(string); ok && v != "" {
		return v
	}
	return *token
}

func setToken(v string) { agentCurrentToken.Store(v) }

// agentStateDir is where the machine keeps its identity. SANDRPOD_STATE_DIR
// overrides it for tests and unusual deployments.
func agentStateDir() string {
	if v := strings.TrimSpace(os.Getenv("SANDRPOD_STATE_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".sandrpod")
}

// resolveAgentToken decides what this agent authenticates with.
//
// A stored key beats the command line: the key is the long-lived credential,
// while the command line keeps whatever was pasted the first time. A `wk_` token
// is used as-is. A `jn_` invitation is exchanged. Anything else (a platform
// token, a self-hosted static token) passes through untouched.
func resolveAgentToken(apiURL, presented, dir string) (string, string) {
	path := filepath.Join(dir, agentCredentialFile)
	cred, ok := readAgentCredential(path)
	if ok && cred.Token != "" && !credentialExpired(cred.ExpiresAt) {
		if presented != "" && strings.HasPrefix(presented, "jn_") {
			log.Printf("worker key (expires %s) takes precedence over the join token on the command line", cred.ExpiresAt)
		} else {
			log.Printf("Using the worker key from %s (expires %s)", path, cred.ExpiresAt)
		}
		return cred.Token, cred.PoderID
	}
	if ok && cred.Token != "" {
		log.Printf("Worker key in %s expired or unreadable (%s); re-enrolling", path, cred.ExpiresAt)
	}

	if !strings.HasPrefix(presented, "jn_") {
		return presented, cred.PoderID
	}
	if apiURL == "" {
		log.Printf("Join token given but no API URL to enrol against; using it as-is")
		return presented, cred.PoderID
	}

	poderID := cred.PoderID
	if poderID == "" {
		poderID = newAgentPoderID()
	}
	key, err := enrolAgent(apiURL, presented, poderID)
	if err != nil {
		log.Printf("Enrolment failed: %v", err)
		log.Printf("  the join token is an invitation: it expires and is exchanged for a worker key " +
			"on first use.")
		log.Printf("  mint a new one in the console (节点 → 添加节点) and restart this agent with it; " +
			"the running agent keeps retrying either way.")
		// Fall back to the invitation: a control plane that predates the enrolment
		// endpoint still accepts join tokens directly, and refusing to start would
		// turn a missing feature into an outage.
		return presented, poderID
	}
	cred = agentCredential{PoderID: poderID, Token: key.Token, ExpiresAt: key.ExpiresAt}
	if err := writeAgentCredential(path, cred); err != nil {
		log.Printf("WARNING: could not persist the worker key to %s (%v); it will not survive a restart", path, err)
	} else {
		log.Printf("Enrolled as agent %s — credential stored in %s, expires %s", poderID, path, key.ExpiresAt)
	}
	return key.Token, poderID
}

func credentialExpired(expires string) bool {
	if expires == "" {
		return false // a static token with no expiry: never expires here
	}
	exp, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return true // unreadable is treated as dead; re-enrol is the safe direction
	}
	return !time.Now().Before(exp.Add(-time.Minute))
}

func newAgentPoderID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// Time-based fallback: the id only has to be stable and unique enough to
		// bind a credential, and crypto/rand failing on a modern OS is already a
		// broken machine.
		return fmt.Sprintf("agent-%d", time.Now().UnixNano())
	}
	return "agent-" + hex.EncodeToString(buf)
}

func readAgentCredential(path string) (agentCredential, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agentCredential{}, false
	}
	var c agentCredential
	if err := json.Unmarshal(data, &c); err != nil || c.Token == "" {
		return agentCredential{}, false
	}
	return c, true
}

func writeAgentCredential(path string, c agentCredential) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// 0600 and an atomic rename: a half-written key file is an agent that cannot
	// authenticate after the next restart.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type agentEnrolment struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// enrolAgent posts the invitation and returns the credential it buys.
func enrolAgent(apiURL, joinToken, poderID string) (agentEnrolment, error) {
	body, err := json.Marshal(map[string]string{"poder_id": poderID})
	if err != nil {
		return agentEnrolment{}, err
	}
	url := strings.TrimSuffix(apiURL, "/") + "/api/v1/poders/enroll"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return agentEnrolment{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+joinToken)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return agentEnrolment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return agentEnrolment{}, fmt.Errorf("control plane answered %s to enrolment (the invitation "+
			"may have expired; mint a new one)", resp.Status)
	}
	var out agentEnrolment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return agentEnrolment{}, err
	}
	if out.Token == "" {
		return agentEnrolment{}, fmt.Errorf("control plane returned no credential")
	}
	return out, nil
}

// agentEnrolRetryInterval is short enough to catch a flaky window and long enough
// not to hammer a control plane that is down.
const agentEnrolRetryInterval = 2 * time.Minute

// retryAgentEnrolment keeps trying to trade the invitation for a worker key.
//
// Enrolment fails for reasons that come and go: a proxy resetting half the new
// connections, a control plane restarting, DNS answering late. Without a retry
// the agent runs on an invitation that expires, and then cannot reconnect at all
// — the sandbox disappears a day after somebody set it up.
//
// It also allows recovery from a revoked key: when the tunnel rejects the key,
// connectLoop puts the invitation back as the current credential, and this loop
// exchanges it again (if it is still valid).
func retryAgentEnrolment(ctx context.Context, apiURL, dir, poderID string) {
	if strings.TrimSpace(apiURL) == "" {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(agentEnrolRetryInterval):
		}
		cur := activeToken()
		if !strings.HasPrefix(cur, "jn_") {
			continue // a key or a static token: nothing to trade
		}
		key, err := enrolAgent(apiURL, cur, poderID)
		if err != nil {
			log.Printf("Enrolment retry failed: %v", err)
			continue
		}
		setToken(key.Token)
		if err := writeAgentCredential(filepath.Join(dir, agentCredentialFile),
			agentCredential{PoderID: poderID, Token: key.Token, ExpiresAt: key.ExpiresAt}); err != nil {
			log.Printf("Enrolled, but could not persist the key (%v); it will not survive a restart", err)
		}
		log.Printf("Enrolled as agent %s on retry — credential stored, expires %s", poderID, key.ExpiresAt)
	}
}
