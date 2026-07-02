// Copyright 2024 SandrPod
// API Server - REST API control plane
// Handles incoming requests and creates jobs; does not connect to cloud providers directly

package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sandrpod/sandrpod/pkg/provider"
	"github.com/sandrpod/sandrpod/pkg/provider/aliyun"
	"github.com/sandrpod/sandrpod/pkg/provider/aws"
	"github.com/sandrpod/sandrpod/pkg/provider/azure"
	"github.com/sandrpod/sandrpod/pkg/provider/digitalocean"
	"github.com/sandrpod/sandrpod/pkg/provider/gcp"
	"github.com/sandrpod/sandrpod/pkg/provider/hetzner"
	"github.com/sandrpod/sandrpod/pkg/provider/oracle"
	"github.com/sandrpod/sandrpod/pkg/provider/tencent"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	sqlitestore "github.com/sandrpod/sandrpod/pkg/store/sqlite"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

var (
	port           = flag.Int("port", 8080, "API server port")
	help           = flag.Bool("help", false, "Show help")
	token          = flag.String("token", os.Getenv("SANDRPOD_TOKEN"), "API token for authentication (env: SANDRPOD_TOKEN)")
	offlineTimeout = flag.Duration("offline-timeout", 30*time.Second, "Poder offline timeout")
	reapTimeout    = flag.Duration("reap-timeout", 10*time.Minute, "OFFLINE poder reclamation timeout (terminates cloud VM and deletes record)")
	dbDSN          = flag.String("db", "", `persistence backend: empty=in-memory (default), sqlite:<path>=SQLite file (e.g. sqlite:./data/sandrpod.db)`)
	publicURL      = flag.String("public-url", os.Getenv("SANDRPOD_PUBLIC_URL"), "Public URL of this API server, used when bootstrapping cloud VMs (e.g. https://api.example.com). Defaults to http://localhost:<port> if not set (env: SANDRPOD_PUBLIC_URL)")
)

// poderTombstones remembers recently-deleted poder IDs so a deleted poder's
// still-dying container can't re-register and leave a ghost OFFLINE record.
// 10 minutes comfortably outlives every cloud's VM-termination window.
var poderTombstones = podpkg.NewTombstones(10 * time.Minute)

// serverConfig holds runtime configuration for the HTTP mux.
type serverConfig struct {
	Token  string // API bearer token; empty = no auth
	APIURL string // used by scheduler for bootstrapping
}

// buildMux constructs and returns the HTTP mux for the API server.
// All handler logic is self-contained here so that tests can call it directly
// without starting a real TCP listener.
func buildMux(cfg serverConfig, stores podpkg.Stores, tunnelStore, directStore *tunnel.TunnelStore) http.Handler {
	jobStore := stores.Jobs
	sandboxStore := stores.Sandboxes
	poderStore := stores.Poders
	scheduler := podpkg.NewScheduler(poderStore, cfg.APIURL, cfg.Token)

	mux := http.NewServeMux()

	wsUpgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// Authentication middleware.
	//
	// Two accepted header schemes:
	//
	//   X-Sandrpod-Token: <cfg.Token>          ← preferred for new clients
	//   Authorization:    Bearer <cfg.Token>   ← legacy fallback
	//
	// Why two? The MCP transport bridge needs the Authorization header
	// to carry a DIFFERENT secret (the agent's --mcp-token) and have it
	// forwarded verbatim through the tunnel. If we kept consuming
	// Authorization for the API token, every MCP client would have to
	// choose between passing API-Server auth or passing agent auth — they
	// can't put two different bearer values in one header. See
	// docs/MCP_AUTH_HEADER_CONFLICT_FIX.md for the full case.
	//
	// New behavior:
	//   - X-Sandrpod-Token correct           → next; Authorization untouched
	//   - X-Sandrpod-Token missing/wrong AND
	//     Authorization correct              → next (legacy path)
	//   - Otherwise                          → 401 with both schemes in
	//                                          WWW-Authenticate
	authMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if cfg.Token == "" {
				next(w, r)
				return
			}

			want := []byte(cfg.Token)

			// Preferred: X-Sandrpod-Token. Constant-time compare prevents
			// the obvious timing oracle on the secret.
			if got := r.Header.Get("X-Sandrpod-Token"); got != "" &&
				subtle.ConstantTimeCompare([]byte(got), want) == 1 {
				next(w, r)
				return
			}

			// Legacy: Authorization: Bearer <cfg.Token>. When this path
			// fires, Authorization will reach the agent as cfg.Token —
			// not the MCP Bearer — but legacy callers don't use /mcp,
			// so the practical impact is zero.
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				if subtle.ConstantTimeCompare([]byte(auth[len("Bearer "):]), want) == 1 {
					next(w, r)
					return
				}
			}

			w.Header().Set("WWW-Authenticate",
				`Bearer realm="sandrpod-api", X-Sandrpod-Token realm="sandrpod-api"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		}
	}

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "ok",
			"version":   "0.3.0",
			"timestamp": time.Now().Unix(),
			"mode":      "control-plane+tunnel",
		})
	})

	// === Poder tunnel entry point ===
	// Poder dials this endpoint on startup to register and establish a yamux reverse tunnel
	mux.HandleFunc("/ws/poder/connect", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		poderID := r.Header.Get("X-Poder-ID")
		if poderID == "" {
			http.Error(w, "X-Poder-ID header required", http.StatusBadRequest)
			return
		}
		// A deleted poder's container often survives its VM by a few seconds
		// and reconnects, leaving a ghost OFFLINE record. Reject tombstoned IDs.
		if poderTombstones.Contains(poderID) {
			log.Printf("Poder %s rejected: recently deleted (tombstoned)", poderID)
			http.Error(w, "poder was deleted", http.StatusGone)
			return
		}

		ws, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Poder %s WebSocket upgrade failed: %v", poderID, err)
			return
		}

		// Parse registration info from headers
		cpuCores, _ := strconv.Atoi(r.Header.Get("X-Poder-CPU-Cores"))
		memBytes, _ := strconv.ParseInt(r.Header.Get("X-Poder-Memory-Bytes"), 10, 64)
		maxContainers, _ := strconv.Atoi(r.Header.Get("X-Poder-Max-Containers"))
		if cpuCores == 0 {
			cpuCores = 4
		}
		if memBytes == 0 {
			memBytes = 8 * 1024 * 1024 * 1024
		}
		if maxContainers == 0 {
			maxContainers = 10
		}

		poderStore.Register(&podpkg.RegisterPoderRequest{
			ID:           poderID,
			Name:         r.Header.Get("X-Poder-Name"),
			URL:          "tunnel://" + poderID, // Marks this as tunnel mode; not used for direct HTTP
			Region:       r.Header.Get("X-Poder-Region"),
			ProviderType: r.Header.Get("X-Poder-Provider-Type"),
			VMID:         r.Header.Get("X-Poder-VM-ID"),
			Resources: podpkg.PoderResources{
				CPUCores:      cpuCores,
				MemoryBytes:   memBytes,
				MaxContainers: maxContainers,
				Arch:          r.Header.Get("X-Poder-Arch"),
				OS:            r.Header.Get("X-Poder-OS"),
				OSVersion:     r.Header.Get("X-Poder-OS-Version"),
				KernelVersion: r.Header.Get("X-Poder-Kernel-Version"),
			},
		})

		t, err := tunnel.NewPoderTunnel(poderID, ws)
		if err != nil {
			log.Printf("Poder %s tunnel creation failed: %v", poderID, err)
			ws.Close()
			return
		}
		tunnelStore.Add(t)
		log.Printf("Poder %s connected via tunnel", poderID)
		// Reconciliation now happens via heartbeat (reconcileByHeartbeat).
		// The first heartbeat from Poder (≤10 s) will carry ContainerNames and
		// mark any stale RUNNING sandboxes as ERROR — no tunnel-HTTP probe needed.

		// Disconnect cleanup: only remove from store if it is still this tunnel (not overwritten by a reconnect)
		defer func() {
			if cur, ok := tunnelStore.Get(poderID); ok && cur == t {
				tunnelStore.Remove(poderID)
				poderStore.SetOffline(poderID)
				// Mark surviving sandboxes as ERROR — the tunnel is gone so we
				// cannot reach the containers; callers will get a meaningful
				// state rather than a stale RUNNING record.
				for _, sb := range sandboxStore.ListByPoderID(poderID) {
					if sb.State == podpkg.StateRunning || sb.State == podpkg.StateStarting {
						_ = sandboxStore.Update(sb.Name, func(s *podpkg.SandboxInfo) {
							s.State = podpkg.StateError
						})
					}
				}
			}
			t.Close()
			log.Printf("Poder %s tunnel disconnected", poderID)
		}()

		// Block until the yamux session closes (internal ping every 3s; returns on failure)
		t.Wait()
	}))

	// === Local agent direct-connect entry point (end-user scenario) ===
	// sandrpod-agent dials this endpoint on startup to register the local machine as a direct sandbox
	mux.HandleFunc("/ws/sandbox/connect", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		sandboxName := r.Header.Get("X-Sandbox-Name")
		if sandboxName == "" {
			http.Error(w, "X-Sandbox-Name header required", http.StatusBadRequest)
			return
		}

		ws, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Agent %s WebSocket upgrade failed: %v", sandboxName, err)
			return
		}

		// Establish a yamux tunnel (API Server acts as yamux client; Agent acts as server)
		t, err := tunnel.NewPoderTunnel(sandboxName, ws)
		if err != nil {
			log.Printf("Agent %s tunnel creation failed: %v", sandboxName, err)
			ws.Close()
			return
		}
		directStore.Set(sandboxName, t)

		// Register or update sandbox metadata
		now := time.Now()
		sb := &podpkg.SandboxInfo{
			ID:           sandboxName,
			Name:         sandboxName,
			Region:       r.Header.Get("X-Sandbox-Region"),
			ProviderType: "local-agent",
			State:        podpkg.StateRunning,
			ProxyURL:     "direct://" + sandboxName,
			Arch:         r.Header.Get("X-Sandbox-Arch"),
			OS:           r.Header.Get("X-Sandbox-OS"),
			OSVersion:    r.Header.Get("X-Sandbox-OS-Version"),
			CreatedAt:    now,
			LastActivity: now,
		}
		// Update if it already exists (agent reconnect), otherwise add
		if existing, ok := sandboxStore.Get(sandboxName); ok {
			sandboxStore.Update(sandboxName, func(s *podpkg.SandboxInfo) {
				s.State = podpkg.StateRunning
				s.ProxyURL = "direct://" + sandboxName
				s.Arch = sb.Arch
				s.OS = sb.OS
				s.OSVersion = sb.OSVersion
				s.LastActivity = now
			})
			_ = existing
		} else {
			sandboxStore.Add(sb)
		}
		log.Printf("Agent %s connected as direct sandbox", sandboxName)

		defer func() {
			// Only mark offline if we are still the active tunnel (guards against
			// a reconnect that registered a new tunnel before this one closed).
			if current, ok := directStore.Get(sandboxName); ok && current == t {
				directStore.Remove(sandboxName)
				sandboxStore.Update(sandboxName, func(s *podpkg.SandboxInfo) {
					s.State = podpkg.StateError
				})
			}
			t.Close()
			log.Printf("Agent %s disconnected", sandboxName)
		}()

		for !t.Closed() {
			time.Sleep(5 * time.Second)
		}
	}))

	// === Poder API ===
	// GET /api/v1/poders - list all Poder nodes
	mux.HandleFunc("/api/v1/poders", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		poders := poderStore.List()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"poders": poders})
	}))

	// /api/v1/poders/* - Poder details, heartbeat, and direct sandbox operations
	mux.HandleFunc("/api/v1/poders/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/api/v1/poders/"):]

		// DELETE /api/v1/poders/{id} — delete a Poder record
		if !strings.Contains(path, "/") && r.Method == http.MethodDelete {
			pID := path
			poder, ok := poderStore.Get(pID)
			if !ok {
				http.Error(w, "poder not found", http.StatusNotFound)
				return
			}
			// Clean up sandboxes that belonged to this Poder.
			// If the tunnel is still alive, ask Poder to delete each container
			// first, then remove the store record. If the tunnel is gone (or
			// the delete request fails), we still remove the record so it
			// doesn't linger as a stale RUNNING entry.
			sandboxes := sandboxStore.ListByPoderID(pID)
			t, tunnelAlive := tunnelStore.Get(pID)
			for _, sb := range sandboxes {
				if tunnelAlive {
					delReq, _ := http.NewRequestWithContext(r.Context(), http.MethodDelete,
						"http://poder/sandboxes/"+sb.Name, nil)
					if resp, err := t.Client.Do(delReq); err != nil {
						log.Printf("DELETE poder %s: failed to delete container %s: %v", pID, sb.Name, err)
					} else {
						resp.Body.Close()
					}
				}
				_ = sandboxStore.Delete(sb.Name)
			}
			// Tombstone BEFORE closing the tunnel: the dying poder container
			// often reconnects within seconds and would re-register a ghost
			// record. Skipped for keep_vm — a kept VM's poder legitimately
			// reconnects.
			keepVM := r.URL.Query().Get("keep_vm") == "true"
			if !keepVM {
				poderTombstones.Add(pID)
			}
			if tunnelAlive {
				t.Close()
			}
			if err := poderStore.Delete(pID); err != nil {
				http.Error(w, fmt.Sprintf("failed to delete poder: %v", err), http.StatusInternalServerError)
				return
			}
			// Terminate the underlying cloud VM for aws/aliyun poders unless the
			// caller opted out with ?keep_vm=true. Failures are logged only.
			vmTerminated := false
			if isCloudProvider(poder.ProviderType) && poder.VMID != "" && !keepVM {
				if p, err := provider.GetFactory().Get(poder.ProviderType); err == nil {
					// VM termination can take minutes (e.g. a GCP instance delete
					// waits ~90s for the operation). Use a detached context so a
					// client disconnect doesn't cancel the delete mid-flight —
					// otherwise the VM leaks and the log shows "context canceled".
					delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					err := p.DeleteVM(delCtx, poder.VMID)
					cancel()
					if err != nil {
						log.Printf("DELETE poder %s: failed to terminate VM %s: %v", pID, poder.VMID, err)
					} else {
						vmTerminated = true
						log.Printf("DELETE poder %s: terminated VM %s", pID, poder.VMID)
					}
				} else {
					log.Printf("DELETE poder %s: provider %q unavailable, VM %s not terminated: %v", pID, poder.ProviderType, poder.VMID, err)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "vm_terminated": vmTerminated})
			return
		}

		// POST /api/v1/poders/{id}/heartbeat
		if pID, ok := strings.CutSuffix(path, "/heartbeat"); ok && r.Method == http.MethodPost {
			var req podpkg.HeartbeatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := poderStore.Heartbeat(pID, &req); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			// Reconcile sandbox states using the authoritative container list.
			// Only act when Poder sends a non-nil list (empty list is valid and
			// means all containers are gone; nil/absent means old Poder version).
			if req.ContainerNames != nil {
				reconcileByHeartbeat(pID, req.ContainerNames, sandboxStore)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			return
		}

		// /api/v1/poders/{id}/sandboxes routes
		if strings.Contains(path, "/sandboxes") {
			parts := strings.SplitN(path, "/", 2)
			if len(parts) < 2 {
				http.NotFound(w, r)
				return
			}
			pID := parts[0]
			rest := parts[1]

			t, ok := tunnelStore.Get(pID)
			if !ok {
				http.Error(w, "poder tunnel not available", http.StatusServiceUnavailable)
				return
			}

			sbParts := strings.SplitN(rest, "/", 2)
			if sbParts[0] != "sandboxes" {
				http.NotFound(w, r)
				return
			}

			// POST /api/v1/poders/{id}/sandboxes - create a sandbox on a specific Poder
			if len(sbParts) == 1 && r.Method == http.MethodPost {
				var req podpkg.CreateSandboxRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				body, _ := json.Marshal(req)
				proxyReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://poder/sandboxes", bytes.NewReader(body))
				proxyReq.Header.Set("Content-Type", "application/json")
				resp, err := t.Client.Do(proxyReq)
				if err != nil {
					http.Error(w, fmt.Sprintf("Failed to create sandbox: %v", err), http.StatusInternalServerError)
					return
				}
				defer resp.Body.Close()
				respBody, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusCreated {
					http.Error(w, fmt.Sprintf("Poder error: %s", string(respBody)), resp.StatusCode)
					return
				}
				var poderResp struct {
					ID    string `json:"id"`
					IP    string `json:"ip"`
					Name  string `json:"name"`
					State string `json:"state"`
				}
				json.Unmarshal(respBody, &poderResp)
				poderArch, poderOS, poderOSVersion := "", "", ""
				// The sandbox runs on this poder, so the poder's provider type is
				// authoritative — prefer it over the request's (which may carry a
				// client default like "local"). Fall back to the request only when
				// the poder has no provider type recorded.
				providerType := req.ProviderType
				if pi, ok := poderStore.Get(pID); ok {
					poderArch = pi.Resources.Arch
					poderOS = pi.Resources.OS
					poderOSVersion = pi.Resources.OSVersion
					if pi.ProviderType != "" {
						providerType = pi.ProviderType
					}
				}
				sandbox := &podpkg.SandboxInfo{
					ID:           poderResp.ID,
					Name:         req.Name,
					Region:       req.Region,
					ProviderType: providerType,
					InstanceType: req.InstanceType,
					State:        podpkg.StateRunning,
					IP:           poderResp.IP,
					PoderID:      pID,
					ProxyURL:     "tunnel://" + pID,
					Arch:         poderArch,
					OS:           poderOS,
					OSVersion:    poderOSVersion,
					CreatedAt:    time.Now(),
				}
				sandboxStore.Add(sandbox)
				poderStore.UpdateUsage(pID, func(u *podpkg.PoderUsage) { u.Containers++ })
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(sandbox)
				return
			}

			// Parse sandbox name and action
			if len(sbParts) < 2 {
				http.NotFound(w, r)
				return
			}
			nameParts := strings.SplitN(sbParts[1], "/", 2)
			sandboxName := nameParts[0]
			sbAction := ""
			if len(nameParts) > 1 {
				sbAction = nameParts[1]
			}

			switch {
			case sbAction == "" && r.Method == http.MethodGet:
				proxyHTTP(t, r, "http://poder/sandboxes/"+sandboxName, w)
			case sbAction == "start" && r.Method == http.MethodPost:
				sb, ok := sandboxStore.Get(sandboxName)
				if !ok {
					http.Error(w, "Sandbox not found", http.StatusNotFound)
					return
				}
				proxyHTTP(t, r, "http://poder/sandboxes/"+sandboxName+"/start", w)
				sandboxStore.Update(sb.Name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateRunning })
			case sbAction == "stop" && r.Method == http.MethodPost:
				sb, ok := sandboxStore.Get(sandboxName)
				if !ok {
					http.Error(w, "Sandbox not found", http.StatusNotFound)
					return
				}
				proxyHTTP(t, r, "http://poder/sandboxes/"+sandboxName+"/stop", w)
				sandboxStore.Update(sb.Name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateStopped })
			case sbAction == "" && r.Method == http.MethodDelete:
				sb, ok := sandboxStore.Get(sandboxName)
				if !ok {
					http.Error(w, "Sandbox not found", http.StatusNotFound)
					return
				}
				if err := proxyHTTPErr(t, r, "http://poder/sandboxes/"+sandboxName, w); err == nil {
					sandboxStore.Delete(sb.Name)
					poderStore.UpdateUsage(pID, func(u *podpkg.PoderUsage) {
						if u.Containers > 0 {
							u.Containers--
						}
					})
				}
			default:
				http.NotFound(w, r)
			}
			return
		}

		// GET /api/v1/poders/{id} - get a single Poder
		if r.Method == http.MethodGet {
			poder, ok := poderStore.Get(path)
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(poder)
			return
		}

		http.NotFound(w, r)
	}))

	// === Sandbox API ===
	mux.HandleFunc("/api/v1/sandboxes", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sandboxes := sandboxStore.List()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"sandboxes": sandboxes})

		case http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
			var req podpkg.CreateSandboxRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if req.ProviderType == "" {
				req.ProviderType = "local"
			}

			// Cloud provisioning can take minutes (VM boot + Docker install +
			// image pulls). Detach it from the request context so a client
			// disconnect/timeout mid-flight doesn't abort provisioning halfway
			// and orphan a half-bootstrapped VM — the flow completes, the
			// Poder registers, and the client can poll `GET /sandboxes/{name}`.
			schedCtx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			job, err := scheduler.ScheduleSandboxCreation(schedCtx, &req)
			cancel()
			if err != nil {
				// Log server-side too: with provisioning detached, the client
				// has often disconnected by the time this fails — writing the
				// error only to a dead connection would leave no trace at all.
				log.Printf("create sandbox %s (provider=%s) failed: %v", req.Name, req.ProviderType, err)
				http.Error(w, fmt.Sprintf("Failed to create sandbox: %v", err), http.StatusInternalServerError)
				return
			}
			if err := jobStore.AddJob(job); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			sbArch, sbOS, sbOSVersion := "", "", ""
			if pi, ok := poderStore.Get(job.PoderID); ok {
				sbArch = pi.Resources.Arch
				sbOS = pi.Resources.OS
				sbOSVersion = pi.Resources.OSVersion
			}
			sandbox := &podpkg.SandboxInfo{
				ID:           job.ID,
				Name:         req.Name,
				Region:       req.Region,
				ProviderType: req.ProviderType,
				InstanceType: req.InstanceType,
				PoderID:      job.PoderID,
				ProxyURL:     "tunnel://" + job.PoderID,
				State:        podpkg.StatePending,
				Arch:         sbArch,
				OS:           sbOS,
				OSVersion:    sbOSVersion,
				CreatedAt:    time.Now(),
			}
			sandboxStore.Add(sandbox)

			// Create the sandbox by calling Poder directly through the tunnel
			t, ok := tunnelStore.Get(job.PoderID)
			if !ok {
				jobStore.UpdateJob(job.ID, func(j *podpkg.Job) {
					j.Status = podpkg.JobStatusFailed
					j.ErrorMessage = "poder tunnel not available"
				})
				sandboxStore.Update(req.Name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateError })
				http.Error(w, "poder tunnel not available", http.StatusServiceUnavailable)
				return
			}

			bodyBytes, _ := json.Marshal(req)
			// Detached like the scheduling step above: after minutes of VM
			// provisioning the client has often already disconnected, and this
			// final container-create must not die on the canceled request
			// context (it would mark the sandbox ERROR with the VM/poder fine).
			poderCtx, poderCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer poderCancel()
			createReq, _ := http.NewRequestWithContext(poderCtx, http.MethodPost, "http://poder/sandboxes", bytes.NewReader(bodyBytes))
			createReq.Header.Set("Content-Type", "application/json")

			resp, err := t.Client.Do(createReq)
			if err != nil {
				jobStore.UpdateJob(job.ID, func(j *podpkg.Job) {
					j.Status = podpkg.JobStatusFailed
					j.ErrorMessage = err.Error()
				})
				sandboxStore.Update(req.Name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateError })
				http.Error(w, fmt.Sprintf("Failed to create sandbox: %v", err), http.StatusInternalServerError)
				return
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusCreated {
				jobStore.UpdateJob(job.ID, func(j *podpkg.Job) {
					j.Status = podpkg.JobStatusFailed
					j.ErrorMessage = string(respBody)
				})
				sandboxStore.Update(req.Name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateError })
				http.Error(w, fmt.Sprintf("Poder error: %s", string(respBody)), resp.StatusCode)
				return
			}

			var poderResp map[string]any
			json.Unmarshal(respBody, &poderResp)

			jobStore.UpdateJob(job.ID, func(j *podpkg.Job) {
				j.Status = podpkg.JobStatusCompleted
				if v, _ := poderResp["id"].(string); v != "" {
					j.SandboxID = v
					j.Result = &podpkg.JobResult{ProxyURL: "tunnel://" + job.PoderID, SandboxID: v}
				}
				if v, _ := poderResp["ip"].(string); v != "" && j.Result != nil {
					j.Result.IP = v
				}
			})
			sandboxStore.Update(req.Name, func(s *podpkg.SandboxInfo) {
				s.State = podpkg.StateRunning
				if v, _ := poderResp["id"].(string); v != "" {
					s.ID = v
				}
				if v, _ := poderResp["ip"].(string); v != "" {
					s.IP = v
				}
			})

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"job_id":  job.ID,
				"status":  "created",
				"sandbox": sandbox,
			})

		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// /api/v1/sandboxes/* - per-sandbox operations and proxy
	mux.HandleFunc("/api/v1/sandboxes/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/api/v1/sandboxes/"):]
		if path == "" {
			http.NotFound(w, r)
			return
		}

		// POST /api/v1/sandboxes/execute - execute code in a sandbox
		if path == "execute" {
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			sandboxName := r.URL.Query().Get("sandbox")
			if sandboxName == "" {
				http.Error(w, "sandbox name is required", http.StatusBadRequest)
				return
			}
			sb, t, ok := sandboxTunnel(sandboxName, sandboxStore, tunnelStore, directStore, w)
			if !ok {
				return
			}
			proxyHTTP(t, r, "http://poder/execute?sandbox="+sandboxName, w)
			_ = sb
			return
		}

		// GET|POST /api/v1/sandboxes/stream - stream execution output
		if path == "stream" {
			if r.Method != http.MethodGet && r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			sandboxName := r.URL.Query().Get("sandbox")
			if sandboxName == "" {
				http.Error(w, "sandbox name is required", http.StatusBadRequest)
				return
			}
			_, t, ok := sandboxTunnel(sandboxName, sandboxStore, tunnelStore, directStore, w)
			if !ok {
				return
			}
			targetURL := "http://poder/stream?" + r.URL.RawQuery
			var body []byte
			if r.Method == http.MethodPost {
				body, _ = io.ReadAll(r.Body)
			}
			req, _ := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
			for k, v := range r.Header {
				if k != "Host" {
					req.Header[k] = v
				}
			}
			resp, err := t.StreamClient().Do(req)
			if err != nil {
				http.Error(w, "failed to stream", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			maps.Copy(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
			return
		}

		// Parse {name} and action from the path
		parts := splitPath(path)
		name := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}

		// Detect session sub-path
		sessionPath := ""
		sessionPrefix := name + "/session"
		if trimmed, ok := strings.CutPrefix(path, sessionPrefix); ok {
			sessionPath = trimmed
			if sessionPath == "" {
				sessionPath = "/"
			}
		}

		// MCP transport bridge proxy: any method, any sub-path under /mcp.
		// The bridge runs inside sandrpod-agent and exposes MCP Streamable
		// HTTP, which negotiates SSE on the same endpoint, so we route
		// through the streaming proxy to preserve real-time flushes.
		// See docs/MCP_TRANSPORT_BRIDGE_DESIGN.md §七.
		if action == "mcp" || strings.HasPrefix(action, "mcp/") {
			_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
			if !ok {
				return
			}
			// Reconstruct the agent-side path: /mcp, /mcp/manifest, /mcp/sse, ...
			subPath := strings.TrimPrefix(path, name+"/")
			targetURL := "http://agent/" + subPath
			if r.URL.RawQuery != "" {
				targetURL += "?" + r.URL.RawQuery
			}
			proxyHTTPStreaming(t, r, targetURL, w)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if sessionPath != "" {
				sb, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				_ = sb
				var targetURL string
				if sessionPath == "/" {
					targetURL = "http://poder/process/session/" + name
				} else {
					targetURL = "http://poder/process/session/" + name + sessionPath
				}
				if r.URL.RawQuery != "" {
					targetURL += "?" + r.URL.RawQuery
				}
				proxyHTTP(t, r, targetURL, w)
				return
			}

			if action == "logs" {
				_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				params := url.Values{}
				params.Set("sandbox", name)
				if tail := r.URL.Query().Get("tail"); tail != "" {
					params.Set("tail", tail)
				}
				proxyHTTP(t, r, "http://poder/logs?"+params.Encode(), w)
				return
			}

			if action == "toolbox" || strings.HasPrefix(action, "toolbox/") {
				_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				targetURL := "http://poder/toolbox/" + name + "/" + toolboxSubPath(r.URL.Path, name)
				if r.URL.RawQuery != "" {
					targetURL += "?" + r.URL.RawQuery
				}
				proxyHTTP(t, r, targetURL, w)
				return
			}

			if strings.HasPrefix(action, "session/") {
				_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				targetURL := "http://poder/process/session/" + action
				if r.URL.RawQuery != "" {
					targetURL += "?" + r.URL.RawQuery
				}
				proxyHTTP(t, r, targetURL, w)
				return
			}

			// PTY WebSocket: must be GET (WebSocket upgrade starts with HTTP GET)
			if action == "pty" {
				sb, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				conn, err := wsUpgrader.Upgrade(w, r, nil)
				if err != nil {
					log.Printf("WebSocket upgrade failed: %v", err)
					return
				}
				defer conn.Close()

				var workerConn *websocket.Conn
				if strings.HasPrefix(sb.ProxyURL, "direct://") {
					// Direct agent: toolbox has no /pty/{name}/connect.
					// Do the two-step manually: POST /pty/create → WS /pty/{sessionId}
					createResp, err := t.Client.Post("http://agent/pty/create?width=80&height=24", "", nil)
					if err != nil || createResp.StatusCode != http.StatusOK {
						log.Printf("PTY create failed for direct agent %s: %v", name, err)
						conn.WriteMessage(websocket.TextMessage, []byte("Failed to create PTY session"))
						return
					}
					defer createResp.Body.Close()
					var ptyResp struct {
						SessionID string `json:"session_id"`
					}
					if err := json.NewDecoder(createResp.Body).Decode(&ptyResp); err != nil || ptyResp.SessionID == "" {
						log.Printf("PTY create bad response for %s: %v", name, err)
						conn.WriteMessage(websocket.TextMessage, []byte("Failed to create PTY session"))
						return
					}
					workerConn, _, err = t.WSDialer.Dial("ws://agent/pty/"+ptyResp.SessionID, nil)
					if err != nil {
						log.Printf("Failed to connect to direct agent PTY session %s: %v", ptyResp.SessionID, err)
						conn.WriteMessage(websocket.TextMessage, []byte("Failed to connect to PTY session"))
						return
					}
				} else {
					// Poder tunnel: Poder handles the two-step internally via /pty/{name}/connect
					workerConn, _, err = t.WSDialer.Dial("ws://poder/pty/"+name+"/connect", nil)
					if err != nil {
						log.Printf("Failed to connect to worker PTY: %v", err)
						conn.WriteMessage(websocket.TextMessage, []byte("Failed to connect to PTY session"))
						return
					}
				}
				defer workerConn.Close()
				proxyWS(conn, workerConn)
				return
			}

			// Return sandbox info
			sb, ok := sandboxStore.Get(name)
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(sb)

		case http.MethodPost:
			if sessionPath != "" {
				_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				body, _ := io.ReadAll(r.Body)
				var targetURL string
				if sessionPath == "/" {
					targetURL = "http://poder/process/session/" + name
				} else {
					cleanPath := strings.TrimPrefix(sessionPath, "/")
					targetURL = "http://poder/process/session/" + name + "/" + cleanPath
				}
				req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := t.Client.Do(req)
				if err != nil {
					http.Error(w, "failed to proxy session", http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				maps.Copy(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				io.Copy(w, resp.Body)
				return
			}

			if action == "toolbox" || strings.HasPrefix(action, "toolbox/") {
				_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				subPath := toolboxSubPath(r.URL.Path, name)
				targetURL := "http://poder/toolbox/" + name + "/" + subPath
				if r.URL.RawQuery != "" {
					targetURL += "?" + r.URL.RawQuery
				}
				proxyHTTP(t, r, targetURL, w)
				return
			}

			if action == "start" {
				sb, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				if strings.HasPrefix(sb.ProxyURL, "direct://") {
					// Local agent is always running; start is a no-op
					sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateRunning })
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"status": "running"})
					return
				}
				sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateStarting })
				if err := proxyHTTPErr(t, r, "http://poder/sandboxes/"+name+"/start", w); err != nil {
					sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateError })
					return
				}
				sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateRunning })
				return
			}

			if action == "stop" {
				sb, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				if strings.HasPrefix(sb.ProxyURL, "direct://") {
					// Local agent cannot be paused; stop is a no-op
					sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateStopped })
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"status": "stopped"})
					return
				}
				sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateStopping })
				if err := proxyHTTPErr(t, r, "http://poder/sandboxes/"+name+"/stop", w); err != nil {
					sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateError })
					return
				}
				sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateStopped })
				return
			}

			http.Error(w, "Action not allowed", http.StatusMethodNotAllowed)

		case http.MethodDelete:
			if sessionPath != "" {
				_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				cleanPath := strings.TrimPrefix(sessionPath, "/")
				targetURL := "http://poder/process/session/" + name + "/" + cleanPath
				proxyHTTP(t, r, targetURL, w)
				return
			}

			if strings.HasPrefix(action, "session/") {
				_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				targetURL := "http://poder/process/session/" + name + "/" + strings.TrimPrefix(action, "session/")
				proxyHTTP(t, r, targetURL, w)
				return
			}

			if action == "toolbox" || strings.HasPrefix(action, "toolbox/") {
				_, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				subPath := toolboxSubPath(r.URL.Path, name)
				targetURL := "http://poder/toolbox/" + name + "/" + subPath
				if r.URL.RawQuery != "" {
					targetURL += "?" + r.URL.RawQuery
				}
				proxyHTTP(t, r, targetURL, w)
				return
			}

			// Look up sandbox record first; 404 if not found.
			sb, ok := sandboxStore.Get(name)
			if !ok {
				http.Error(w, "sandbox not found", http.StatusNotFound)
				return
			}

			if strings.HasPrefix(sb.ProxyURL, "direct://") {
				// Direct-agent sandbox: remove record and disconnect agent tunnel.
				sandboxStore.Delete(name)
				directStore.Remove(name)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
				return
			}

			// Best-effort: delete the DB record unconditionally so the name is
			// freed even when the poder tunnel is gone (e.g. after a restart).
			pID := sb.PoderID
			sandboxStore.Delete(name)
			poderStore.UpdateUsage(pID, func(u *podpkg.PoderUsage) {
				if u.Containers > 0 {
					u.Containers--
				}
			})

			// Attempt to tear down the container via the poder tunnel.
			// Log but do not fail the HTTP response if the tunnel is unavailable.
			t, tunnelOK := tunnelStore.Get(pID)
			if tunnelOK {
				req, _ := http.NewRequestWithContext(r.Context(), http.MethodDelete, "http://poder/sandboxes/"+name, nil)
				if resp, err := t.Client.Do(req); err != nil {
					log.Printf("sandbox delete: poder cleanup error for %s: %v", name, err)
				} else {
					resp.Body.Close()
				}
			} else {
				log.Printf("sandbox delete: poder tunnel %s unavailable, skipping container cleanup for %s", pID, name)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
		}
	}))

	// === Job API (called by Poder Agent) ===
	mux.HandleFunc("/api/v1/jobs/poll", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobs, err := jobStore.PollJobs(30*time.Second, 10)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
	}))

	mux.HandleFunc("/api/v1/jobs/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/jobs/"):]
		if jobID == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPatch {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
		var req podpkg.UpdateJobStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Phase 1: update the job under the job store's lock.
		// Do NOT touch sandboxStore here to avoid cross-store lock ordering.
		var sandboxName string
		var jobType podpkg.JobType
		err := jobStore.UpdateJob(jobID, func(job *podpkg.Job) {
			job.Status = req.Status
			if req.ErrorMessage != "" {
				job.ErrorMessage = req.ErrorMessage
			}
			if req.Result != nil {
				job.Result = req.Result
			}
			sandboxName = job.SandboxName
			jobType = job.Type
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		// Phase 2: update sandbox state under the sandbox store's own lock.
		if req.Status == podpkg.JobStatusCompleted || req.Status == podpkg.JobStatusFailed {
			sandboxStore.Update(sandboxName, func(sb *podpkg.SandboxInfo) {
				switch jobType {
				case podpkg.JobTypeCreateSandbox:
					if req.Status == podpkg.JobStatusCompleted {
						sb.State = podpkg.StateRunning
						if req.Result != nil {
							sb.IP = req.Result.IP
							if req.Result.SandboxID != "" {
								sb.ID = req.Result.SandboxID
							}
						}
					} else {
						sb.State = podpkg.StateError
					}
				case podpkg.JobTypeDeleteSandbox:
					if req.Status == podpkg.JobStatusCompleted {
						sb.State = podpkg.StateTerminated
					} else {
						sb.State = podpkg.StateError
					}
				case podpkg.JobTypeStartSandbox:
					if req.Status == podpkg.JobStatusCompleted {
						sb.State = podpkg.StateRunning
						if req.Result != nil && req.Result.IP != "" {
							sb.IP = req.Result.IP
						}
					} else {
						sb.State = podpkg.StateError
					}
				case podpkg.JobTypeStopSandbox:
					if req.Status == podpkg.JobStatusCompleted {
						sb.State = podpkg.StateStopped
					} else {
						sb.State = podpkg.StateError
					}
				}
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))

	return mux
}

func main() {
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	log.Printf("Starting SandrPod API Server v0.3.0 (Control Plane)")

	// Register cloud providers
	if err := aws.Register(); err != nil {
		log.Printf("Warning: Failed to register AWS provider: %v", err)
	} else {
		log.Printf("AWS provider registered")
	}
	if err := aliyun.Register(); err != nil {
		log.Printf("Warning: Failed to register Aliyun provider: %v", err)
	} else {
		log.Printf("Aliyun provider registered")
	}
	if err := azure.Register(); err != nil {
		log.Printf("Warning: Failed to register Azure provider: %v", err)
	} else {
		log.Printf("Azure provider registered")
	}
	if err := gcp.Register(); err != nil {
		log.Printf("Warning: Failed to register GCP provider: %v", err)
	} else {
		log.Printf("GCP provider registered")
	}
	if err := tencent.Register(); err != nil {
		log.Printf("Warning: Failed to register Tencent provider: %v", err)
	} else {
		log.Printf("Tencent provider registered")
	}
	if err := digitalocean.Register(); err != nil {
		log.Printf("Warning: Failed to register DigitalOcean provider: %v", err)
	} else {
		log.Printf("DigitalOcean provider registered")
	}
	if err := hetzner.Register(); err != nil {
		log.Printf("Warning: Failed to register Hetzner provider: %v", err)
	} else {
		log.Printf("Hetzner provider registered")
	}
	if err := oracle.Register(); err != nil {
		log.Printf("Warning: Failed to register Oracle provider: %v", err)
	} else {
		log.Printf("Oracle provider registered")
	}

	factory := provider.GetFactory()
	log.Printf("Registered providers: %v", factory.Names())

	// ── Store construction ────────────────────────────────────────────────────
	var stores podpkg.Stores
	switch {
	case *dbDSN == "":
		stores = store.NewMemoryStores()
		log.Printf("Using in-memory store (data will be lost on restart)")
	case strings.HasPrefix(*dbDSN, "sqlite:"):
		dsn, _ := strings.CutPrefix(*dbDSN, "sqlite:")
		db, err := sqlitestore.Open(dsn)
		if err != nil {
			log.Fatalf("Failed to open SQLite DB %q: %v", dsn, err)
		}
		defer db.Close()
		stores = podpkg.Stores{
			Sandboxes: sqlitestore.NewSandboxRepo(db),
			Poders:    sqlitestore.NewPoderRepo(db),
			Jobs:      sqlitestore.NewJobRepo(db),
		}
		log.Printf("Using SQLite store at %q", dsn)
	default:
		log.Fatalf("Unknown -db value %q (supported: sqlite:<path>)", *dbDSN)
	}

	tunnelStore := tunnel.NewTunnelStore()
	directStore := tunnel.NewDirectTunnelStore() // direct sandbox agent tunnels (toC)
	apiURL := *publicURL
	if apiURL == "" {
		apiURL = fmt.Sprintf("http://localhost:%d", *port)
	}
	cfg := serverConfig{Token: *token, APIURL: apiURL}
	handler := buildMux(cfg, stores, tunnelStore, directStore)

	// Start the HTTP server
	addr := fmt.Sprintf(":%d", *port)
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down...")
		rootCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	go cleanupOfflinePoders(rootCtx, stores.Poders, *offlineTimeout)
	go reapOfflinePoders(rootCtx, stores.Poders, *reapTimeout)

	log.Printf("API server listening on %s (Control Plane + Tunnel Mode)", addr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

// sandboxTunnel looks up a sandbox and its Poder tunnel together.
// Writes an appropriate HTTP error and returns false if either is missing.
func sandboxTunnel(name string, ss podpkg.SandboxRepository, ts *tunnel.TunnelStore, ds *tunnel.TunnelStore, w http.ResponseWriter) (*podpkg.SandboxInfo, *tunnel.PoderTunnel, bool) {
	sb, ok := ss.Get(name)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return nil, nil, false
	}
	// Direct-agent path (end-user): sandbox is registered directly by the local sandrpod-agent
	if strings.HasPrefix(sb.ProxyURL, "direct://") {
		t, ok := ds.Get(name)
		if !ok {
			http.Error(w, "sandbox agent not connected", http.StatusServiceUnavailable)
			return nil, nil, false
		}
		return sb, t, true
	}
	// Poder path (business): sandbox runs in a container managed by Poder
	t, ok := ts.Get(sb.PoderID)
	if !ok {
		http.Error(w, "poder tunnel not available", http.StatusServiceUnavailable)
		return nil, nil, false
	}
	return sb, t, true
}

// proxyHTTP forwards an HTTP request through the tunnel and copies the response.
func proxyHTTP(t *tunnel.PoderTunnel, r *http.Request, targetURL string, w http.ResponseWriter) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	for k, v := range r.Header {
		if k != "Host" {
			req.Header[k] = v
		}
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		log.Printf("tunnel proxy %s error: %v", targetURL, err)
		http.Error(w, "failed to proxy request", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyHTTPStreaming forwards an HTTP request through the tunnel like
// proxyHTTP, but uses the tunnel's streaming client (no buffering, no idle
// timeout) and flushes the response writer after every chunk so SSE events
// reach the caller in real time. Used for MCP Streamable HTTP, which can
// upgrade a single POST into a long-lived text/event-stream response.
func proxyHTTPStreaming(t *tunnel.PoderTunnel, r *http.Request, targetURL string, w http.ResponseWriter) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	for k, v := range r.Header {
		if k != "Host" {
			req.Header[k] = v
		}
	}
	resp, err := t.StreamClient().Do(req)
	if err != nil {
		log.Printf("tunnel streaming proxy %s error: %v", targetURL, err)
		http.Error(w, "failed to proxy request", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// proxyHTTPErr is like proxyHTTP but returns an error if the upstream failed.
func proxyHTTPErr(t *tunnel.PoderTunnel, r *http.Request, targetURL string, w http.ResponseWriter) error {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return err
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		http.Error(w, "failed to proxy request", http.StatusBadGateway)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return fmt.Errorf("upstream %d", resp.StatusCode)
	}
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	return nil
}

// proxyWS bidirectionally proxies two WebSocket connections until either closes.
func proxyWS(client, upstream *websocket.Conn) {
	done := make(chan struct{}, 2)
	relay := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			mt, msg, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}
	go relay(upstream, client)
	go relay(client, upstream)
	<-done
}

// toolboxSubPath extracts the toolbox sub-path from a request URL path.
// e.g. "/api/v1/sandboxes/my-box/toolbox/files/foo" → "files/foo"
func toolboxSubPath(fullPath, sandboxName string) string {
	prefix := "/api/v1/sandboxes/" + sandboxName + "/toolbox/"
	sub := strings.TrimPrefix(fullPath, prefix)
	if sub == fullPath {
		sub = strings.TrimPrefix(fullPath, "/api/v1/sandboxes/"+sandboxName+"/toolbox")
	}
	if sub == "" || sub == fullPath {
		return "files"
	}
	return sub
}

// splitPath splits a URL path by "/".
func splitPath(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(strings.Trim(path, "/"), "/")
}

// reconcileByHeartbeat reconciles sandbox states for a Poder node using the
// authoritative container-name list from the heartbeat payload.
//
//   - RUNNING/STARTING not in list  → ERROR   (container gone)
//   - ERROR but container IS in list → RUNNING (Poder reconnected; container survived)
//
// The second case handles the common upgrade scenario: the tunnel disconnect handler
// marks sandboxes ERROR when Poder temporarily disconnects, but the containers may
// still be running. The first heartbeat after reconnect restores their state.
func reconcileByHeartbeat(poderID string, containerNames []string, ss podpkg.SandboxRepository) {
	// Build a fast lookup set.
	alive := make(map[string]struct{}, len(containerNames))
	for _, name := range containerNames {
		alive[name] = struct{}{}
	}

	for _, sb := range ss.ListByPoderID(poderID) {
		_, isAlive := alive[sb.Name]

		switch sb.State {
		case podpkg.StateRunning, podpkg.StateStarting:
			if !isAlive {
				log.Printf("reconcile %s/%s: container not in heartbeat list → marking ERROR", poderID, sb.Name)
				_ = ss.Update(sb.Name, func(s *podpkg.SandboxInfo) {
					s.State = podpkg.StateError
				})
			}
		case podpkg.StateError:
			if isAlive {
				// Container survived the Poder restart — restore to RUNNING.
				log.Printf("reconcile %s/%s: container alive after reconnect → restoring RUNNING", poderID, sb.Name)
				_ = ss.Update(sb.Name, func(s *podpkg.SandboxInfo) {
					s.State = podpkg.StateRunning
				})
			}
		}
	}
}

// cleanupOfflinePoders marks Poder nodes as offline when heartbeats stop.
// Serves as a safety net alongside the tunnel disconnect handler.
// Exits when ctx is cancelled.
func cleanupOfflinePoders(ctx context.Context, ps podpkg.PoderRepository, timeout time.Duration) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			for _, p := range ps.List() {
				if p.State == podpkg.PoderStateOnline && now.Sub(p.LastHeartbeat) > timeout {
					ps.SetOffline(p.ID)
					log.Printf("Poder %s marked OFFLINE (no heartbeat for %v)", p.ID, now.Sub(p.LastHeartbeat))
				}
			}
		}
	}
}

// isCloudProvider reports whether a provider type backs each Poder with a
// dedicated cloud VM that should be terminated on reclamation.
func isCloudProvider(providerType string) bool {
	switch providerType {
	case "aws", "aliyun", "azure", "gcp", "tencent", "digitalocean", "hetzner", "oracle":
		return true
	default:
		return false
	}
}

// reapOfflinePoders reclaims poders that have been OFFLINE longer than timeout.
// Cloud poders (aws/aliyun) with a known VM ID have their VM terminated before
// the record is deleted; if termination fails the record is kept so the next
// tick retries. Local/docker poders have no VM and are deleted directly.
// Exits when ctx is cancelled.
func reapOfflinePoders(ctx context.Context, ps podpkg.PoderRepository, timeout time.Duration) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			for _, p := range ps.List() {
				if p.State != podpkg.PoderStateOffline || now.Sub(p.LastHeartbeat) <= timeout {
					continue
				}
				if isCloudProvider(p.ProviderType) && p.VMID != "" {
					prov, err := provider.GetFactory().Get(p.ProviderType)
					if err != nil {
						log.Printf("reap poder %s: provider %q unavailable, retrying next tick: %v", p.ID, p.ProviderType, err)
						continue
					}
					if err := prov.DeleteVM(ctx, p.VMID); err != nil {
						log.Printf("reap poder %s: failed to terminate VM %s, retrying next tick: %v", p.ID, p.VMID, err)
						continue
					}
					log.Printf("reap poder %s: terminated VM %s", p.ID, p.VMID)
				}
				// Tombstone so a lingering container can't re-register a ghost.
				poderTombstones.Add(p.ID)
				if err := ps.Delete(p.ID); err != nil {
					log.Printf("reap poder %s: failed to delete record: %v", p.ID, err)
					continue
				}
				log.Printf("reap poder %s: reclaimed (OFFLINE for %v)", p.ID, now.Sub(p.LastHeartbeat))
			}
		}
	}
}
