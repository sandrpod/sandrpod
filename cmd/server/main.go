// Copyright 2024 SandrPod
// API Server - REST API control plane
// Handles incoming requests and creates jobs; does not connect to cloud providers directly

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httputil"
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
	sqldbstore "github.com/sandrpod/sandrpod/pkg/store/sqldb"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

var (
	port           = flag.Int("port", 8080, "API server port")
	help           = flag.Bool("help", false, "Show help")
	token          = flag.String("token", os.Getenv("SANDRPOD_TOKEN"), "API token for authentication (env: SANDRPOD_TOKEN)")
	offlineTimeout = flag.Duration("offline-timeout", 30*time.Second, "Poder offline timeout")
	reapTimeout    = flag.Duration("reap-timeout", 10*time.Minute, "OFFLINE poder reclamation timeout (terminates cloud VM and deletes record)")
	dbDSN          = flag.String("db", "", `persistence backend: empty=in-memory (default); sqlite:<path> (e.g. sqlite:./data/sandrpod.db); postgres://user:pass@host:5432/db?sslmode=require (production)`)
	sandboxIdleTTL = flag.Duration("sandbox-idle-timeout", envDuration("SANDRPOD_SANDBOX_IDLE_TIMEOUT"), "Reap sandboxes idle longer than this, e.g. 2h (0 = disabled; env: SANDRPOD_SANDBOX_IDLE_TIMEOUT)")
	poderIdleTTL   = flag.Duration("poder-idle-timeout", envDuration("SANDRPOD_PODER_IDLE_TIMEOUT"), "Reclaim cloud poders with no sandboxes after this, terminating the VM, e.g. 30m (0 = disabled; env: SANDRPOD_PODER_IDLE_TIMEOUT)")
	publicURL      = flag.String("public-url", os.Getenv("SANDRPOD_PUBLIC_URL"), "Public URL of this API server, used when bootstrapping cloud VMs (e.g. https://api.example.com). Defaults to http://localhost:<port> if not set (env: SANDRPOD_PUBLIC_URL)")
	nodeURL        = flag.String("node-url", os.Getenv("SANDRPOD_NODE_URL"), "Internal URL of THIS instance that peer instances reach it at (e.g. http://10.0.1.5:8080). Set on every instance in a load-balanced, PostgreSQL-backed multi-instance deployment so requests can be forwarded to the node holding a poder's tunnel. Empty = single-instance (no forwarding). (env: SANDRPOD_NODE_URL)")
	tokensFile     = flag.String("tokens-file", os.Getenv("SANDRPOD_TOKENS_FILE"), "JSON file of named API tokens [{name,token,role}] adding to -token; role admin|user; hot-reloaded on change (env: SANDRPOD_TOKENS_FILE)")
	tlsCert        = flag.String("tls-cert", os.Getenv("SANDRPOD_TLS_CERT"), "TLS certificate file; with -tls-key serves HTTPS (env: SANDRPOD_TLS_CERT)")
	tlsKey         = flag.String("tls-key", os.Getenv("SANDRPOD_TLS_KEY"), "TLS private key file (env: SANDRPOD_TLS_KEY)")
	rateLimit      = flag.Float64("rate-limit", float64(envInt("SANDRPOD_RATE_LIMIT")), "Requests/second per user token, 0 = unlimited (env: SANDRPOD_RATE_LIMIT)")
	maxPerOwner    = flag.Int("max-sandboxes-per-owner", envInt("SANDRPOD_MAX_SANDBOXES_PER_OWNER"), "Max concurrent sandboxes per user token (0 = unlimited, admins exempt; env: SANDRPOD_MAX_SANDBOXES_PER_OWNER)")
)

// poderTombstones remembers recently-deleted poder IDs so a deleted poder's
// still-dying container can't re-register and leave a ghost OFFLINE record.
// 10 minutes comfortably outlives every cloud's VM-termination window.
var poderTombstones = podpkg.NewTombstones(10 * time.Minute)

// apiRateLimit, when non-nil, throttles user-role tokens (see -rate-limit).
var apiRateLimit *rateLimiter

// envInt parses an integer env var, returning 0 when unset or malformed.
func envInt(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("Warning: ignoring invalid %s=%q: %v", key, v, err)
		return 0
	}
	return n
}

// envDuration parses a duration env var, returning 0 (disabled) when unset or
// malformed. Used as the flag default so env and flag configure the same knob.
func envDuration(key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("Warning: ignoring invalid %s=%q: %v", key, v, err)
		return 0
	}
	return d
}

// serverConfig holds runtime configuration for the HTTP mux.
type serverConfig struct {
	Token  string // legacy single API bearer token (implicit admin); empty = none
	Tokens []NamedToken
	// Registry, when set, supplies hot-reloadable named tokens (tokens-file).
	Registry *tokenRegistry
	// Keys, when set, is the in-memory index of DB-issued API tokens (matched by
	// hash). Backed by stores.Tokens; loaded at startup, updated on issue/revoke.
	Keys   *apiKeyIndex
	APIURL string // used by scheduler for bootstrapping
	// NodeURL is this instance's internal address for inter-node forwarding in a
	// multi-instance deployment; empty means single-instance (no forwarding).
	NodeURL string
	// TokenStore is the persistent token repository, consulted as a fallback in
	// multi-instance mode so a token issued on a peer instance (absent from this
	// instance's in-memory Keys index) still authenticates.
	TokenStore podpkg.APITokenRepository
	// MaxSandboxesPerOwner caps how many sandboxes a user-role token may hold
	// at once (0 = unlimited; admins are exempt).
	MaxSandboxesPerOwner int
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
			// Auth disabled: everything runs as an anonymous admin (legacy).
			if cfg.authDisabled() {
				next(w, withIdentity(r, identity{Name: "", Role: roleAdmin}))
				return
			}

			// Resolve the presented credential against the legacy single
			// token (implicit admin) and the named-tokens file. Constant-time
			// compares prevent the obvious timing oracle on the secrets.
			// Header forms: X-Sandrpod-Token (preferred) or Bearer (legacy —
			// when the Bearer path fires, Authorization reaches the agent as
			// the platform token, not an MCP Bearer; legacy callers don't use
			// /mcp so the practical impact is zero).
			if id, ok := resolveRequest(cfg, r); ok {
				// Per-identity rate limit for user tokens (admins/poders exempt).
				if apiRateLimit != nil && !id.isAdmin() && !apiRateLimit.allow(id.Name) {
					http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
					return
				}
				next(w, withIdentity(r, id))
				return
			}

			w.Header().Set("WWW-Authenticate",
				`Bearer realm="sandrpod-api", X-Sandrpod-Token realm="sandrpod-api"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		}
	}

	// adminOnly gates infrastructure endpoints (poder/agent registration, job
	// polling, poder management) to admin-role tokens. User tokens only get
	// the sandbox API, scoped to their own sandboxes.
	adminOnly := func(next http.HandlerFunc) http.HandlerFunc {
		return authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			if !identityFrom(r).isAdmin() {
				http.Error(w, "Forbidden: admin token required", http.StatusForbidden)
				return
			}
			next(w, r)
		})
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

	// Prometheus metrics (admin-gated; when auth is off it's public like /health).
	mux.HandleFunc("/metrics", adminOnly(metricsHandler(&stores)))

	// Web console (static SPA; it authenticates its own XHRs with a token the
	// operator pastes in, so the page itself is served unauthenticated).
	mux.HandleFunc("/console", consoleHandler)

	// === Poder tunnel entry point ===
	// Poder dials this endpoint on startup to register and establish a yamux reverse tunnel
	mux.HandleFunc("/ws/poder/connect", adminOnly(func(w http.ResponseWriter, r *http.Request) {
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
		// Multi-instance: record that this node holds the poder's tunnel so peer
		// instances can forward requests here.
		if cfg.NodeURL != "" {
			_ = stores.TunnelOwners.Claim(poderID, cfg.NodeURL)
		}
		log.Printf("Poder %s connected via tunnel", poderID)
		notifyEvent("poder.registered", map[string]any{"poder_id": poderID, "provider": r.Header.Get("X-Poder-Provider-Type"), "vm_id": r.Header.Get("X-Poder-VM-ID")})
		// Reconciliation now happens via heartbeat (reconcileByHeartbeat).
		// The first heartbeat from Poder (≤10 s) will carry ContainerNames and
		// mark any stale RUNNING sandboxes as ERROR — no tunnel-HTTP probe needed.

		// Disconnect cleanup: only remove from store if it is still this tunnel (not overwritten by a reconnect)
		defer func() {
			if cur, ok := tunnelStore.Get(poderID); ok && cur == t {
				tunnelStore.Remove(poderID)
				if cfg.NodeURL != "" {
					_ = stores.TunnelOwners.Release(poderID, cfg.NodeURL)
				}
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
	mux.HandleFunc("/ws/sandbox/connect", adminOnly(func(w http.ResponseWriter, r *http.Request) {
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
		if cfg.NodeURL != "" {
			_ = stores.TunnelOwners.Claim(sandboxName, cfg.NodeURL)
		}

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
				if cfg.NodeURL != "" {
					_ = stores.TunnelOwners.Release(sandboxName, cfg.NodeURL)
				}
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
	mux.HandleFunc("/api/v1/poders", adminOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		poders := poderStore.List()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"poders": poders})
	}))

	// /api/v1/tokens - issue (POST) / list (GET) API tokens; admin only.
	// Issued keys are e2b_<hex> so they drop straight into E2B_API_KEY.
	mux.HandleFunc("/api/v1/tokens", adminOnly(handleTokens(cfg, stores.Tokens)))
	// /api/v1/tokens/{prefix} - revoke a token by its display prefix; admin only.
	mux.HandleFunc("/api/v1/tokens/", adminOnly(handleTokenDelete(cfg, stores.Tokens)))

	// /api/v1/poders/* - Poder details, heartbeat, and direct sandbox operations
	mux.HandleFunc("/api/v1/poders/", adminOnly(func(w http.ResponseWriter, r *http.Request) {
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
			notifyEvent("poder.deleted", map[string]any{"poder_id": pID, "vm_terminated": vmTerminated})
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
					LastActivity: time.Now(),
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
		ident := identityFrom(r)
		switch r.Method {
		case http.MethodGet:
			sandboxes := sandboxStore.List()
			if !ident.isAdmin() {
				visible := make([]*podpkg.SandboxInfo, 0, len(sandboxes))
				for _, sb := range sandboxes {
					if canAccessSandbox(ident, sb) {
						visible = append(visible, sb)
					}
				}
				sandboxes = visible
			}
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

			if _, exists := sandboxStore.Get(req.Name); exists {
				http.Error(w, fmt.Sprintf("sandbox %s already exists", req.Name), http.StatusConflict)
				return
			}

			// Per-owner quota (admins exempt; 0 = unlimited).
			if !ident.isAdmin() && cfg.MaxSandboxesPerOwner > 0 {
				owned := 0
				for _, sb := range sandboxStore.List() {
					if sb.Owner == ident.Name {
						owned++
					}
				}
				if owned >= cfg.MaxSandboxesPerOwner {
					http.Error(w, fmt.Sprintf("sandbox quota reached (%d)", cfg.MaxSandboxesPerOwner), http.StatusTooManyRequests)
					return
				}
			}

			// Async path: register a job + PENDING record, provision in the
			// background, return immediately. Poll GET /api/v1/jobs/{id} or the
			// sandbox state. Cloud provisioning takes minutes and long
			// synchronous responses are routinely killed by intermediate proxies.
			if req.Async {
				job := &podpkg.Job{
					ID:           podpkg.GenerateJobID(),
					Type:         podpkg.JobTypeCreateSandbox,
					Status:       podpkg.JobStatusPending,
					SandboxName:  req.Name,
					Region:       req.Region,
					ProviderType: req.ProviderType,
					InstanceType: req.InstanceType,
					ImageID:      req.ImageID,
					Owner:        ident.Name,
					CreatedAt:    time.Now(),
					UpdatedAt:    time.Now(),
				}
				if err := jobStore.AddJob(job); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				sandbox := &podpkg.SandboxInfo{
					ID:           job.ID,
					Name:         req.Name,
					Region:       req.Region,
					ProviderType: req.ProviderType,
					InstanceType: req.InstanceType,
					State:        podpkg.StatePending,
					Owner:        ident.Name,
					TTLSeconds:   req.TTLSeconds,
					CreatedAt:    time.Now(),
					LastActivity: time.Now(),
				}
				if err := sandboxStore.Add(sandbox); err != nil {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				go func(req podpkg.CreateSandboxRequest, jobID, owner string) {
					if _, _, err := runSandboxCreate(scheduler, sandboxStore, poderStore, jobStore, tunnelStore, &req, jobID, owner); err != nil {
						log.Printf("async create %s: %v", req.Name, err)
					}
				}(req, job.ID, ident.Name)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				json.NewEncoder(w).Encode(map[string]any{
					"job_id":  job.ID,
					"status":  "provisioning",
					"sandbox": sandbox,
				})
				return
			}

			// Synchronous path (legacy default): same flow, inline.
			sandbox, jobID, err := runSandboxCreate(scheduler, sandboxStore, poderStore, jobStore, tunnelStore, &req, "", ident.Name)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"job_id":  jobID,
				"status":  "created",
				"sandbox": sandbox,
			})

		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// /api/v1/sandboxes/* - per-sandbox operations and proxy
	mux.HandleFunc("/api/v1/sandboxes/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		ident := identityFrom(r)
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
			if sb, ok := sandboxStore.Get(sandboxName); ok && !canAccessSandbox(ident, sb) {
				http.Error(w, "sandbox not found", http.StatusNotFound)
				return
			}
			sb, t, ok := sandboxTunnel(sandboxName, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
			if sb, ok := sandboxStore.Get(sandboxName); ok && !canAccessSandbox(ident, sb) {
				http.Error(w, "sandbox not found", http.StatusNotFound)
				return
			}
			_, t, ok := sandboxTunnel(sandboxName, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
			// Flush each chunk so streamed output reaches the client in real time
			// — a plain io.Copy here would re-buffer the toolbox/poder stream.
			flushCopy(w, resp.Body)
			return
		}

		// Parse {name} and action from the path
		parts := splitPath(path)
		name := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}

		// Ownership: user-role tokens only reach their own sandboxes (missing
		// records fall through to the per-action 404s).
		if sb, ok := sandboxStore.Get(name); ok && !canAccessSandbox(ident, sb) {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
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
			_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				sb, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				sb, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				sb, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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
				sb, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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

			if action == "snapshot" {
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
				if !ok {
					return
				}
				targetURL := "http://poder/sandboxes/" + name + "/snapshot"
				if r.URL.RawQuery != "" {
					targetURL += "?" + r.URL.RawQuery
				}
				proxyHTTP(t, r, targetURL, w)
				return
			}

			http.Error(w, "Action not allowed", http.StatusMethodNotAllowed)

		case http.MethodDelete:
			if sessionPath != "" {
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
				if !ok {
					return
				}
				cleanPath := strings.TrimPrefix(sessionPath, "/")
				targetURL := "http://poder/process/session/" + name + "/" + cleanPath
				proxyHTTP(t, r, targetURL, w)
				return
			}

			if strings.HasPrefix(action, "session/") {
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
				if !ok {
					return
				}
				targetURL := "http://poder/process/session/" + name + "/" + strings.TrimPrefix(action, "session/")
				proxyHTTP(t, r, targetURL, w)
				return
			}

			if action == "toolbox" || strings.HasPrefix(action, "toolbox/") {
				_, t, ok := sandboxTunnel(name, r, sandboxStore, tunnelStore, directStore, stores.TunnelOwners, cfg.NodeURL, w)
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

			notifyEvent("sandbox.deleted", map[string]any{"name": name})
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
		}
	}))

	// === Job API (called by Poder Agent) ===
	mux.HandleFunc("/api/v1/jobs/poll", adminOnly(func(w http.ResponseWriter, r *http.Request) {
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
		// GET /api/v1/jobs/{id} — user-facing job status (async create polling).
		if r.Method == http.MethodGet {
			job, ok := jobStore.GetJob(jobID)
			if !ok || !canAccessJob(identityFrom(r), job) {
				http.Error(w, "job not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(job)
			return
		}
		if r.Method != http.MethodPatch {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// PATCH is the poder agent's job-status report — admin tokens only.
		if !identityFrom(r).isAdmin() {
			http.Error(w, "Forbidden: admin token required", http.StatusForbidden)
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
	case strings.HasPrefix(*dbDSN, "sqlite:"),
		strings.HasPrefix(*dbDSN, "postgres://"),
		strings.HasPrefix(*dbDSN, "postgresql://"):
		db, err := sqldbstore.Open(*dbDSN)
		if err != nil {
			log.Fatalf("Failed to open DB: %v", err)
		}
		defer db.Close()
		stores = podpkg.Stores{
			Sandboxes:    sqldbstore.NewSandboxRepo(db),
			Poders:       sqldbstore.NewPoderRepo(db),
			Jobs:         sqldbstore.NewJobRepo(db),
			Tokens:       sqldbstore.NewTokenRepo(db),
			TunnelOwners: sqldbstore.NewTunnelOwnerRepo(db),
		}
		kind := "SQLite"
		if !strings.HasPrefix(*dbDSN, "sqlite:") {
			kind = "PostgreSQL"
		}
		log.Printf("Using %s store", kind)
	default:
		log.Fatalf("Unknown -db value %q (supported: sqlite:<path>, postgres://...)", *dbDSN)
	}

	tunnelStore := tunnel.NewTunnelStore()
	directStore := tunnel.NewDirectTunnelStore() // direct sandbox agent tunnels (toC)
	apiURL := *publicURL
	if apiURL == "" {
		apiURL = fmt.Sprintf("http://localhost:%d", *port)
	}
	cfg := serverConfig{Token: *token, APIURL: apiURL, MaxSandboxesPerOwner: *maxPerOwner, NodeURL: *nodeURL}
	if *nodeURL != "" {
		log.Printf("Multi-instance mode: this node = %s (inter-node forwarding enabled)", *nodeURL)
	}
	if *tokensFile != "" {
		toks, err := loadTokensFile(*tokensFile)
		if err != nil {
			log.Fatalf("Failed to load tokens file %q: %v", *tokensFile, err)
		}
		reg := &tokenRegistry{}
		reg.set(toks)
		cfg.Registry = reg
		log.Printf("Loaded %d named token(s) from %s (hot-reload enabled)", len(toks), *tokensFile)
	}
	if *rateLimit > 0 {
		apiRateLimit = newRateLimiter(*rateLimit)
		log.Printf("Rate limiting enabled: %.1f req/s per user token", *rateLimit)
	}
	// Load DB-issued API tokens into the in-memory auth index (hot path).
	if stores.Tokens != nil {
		cfg.TokenStore = stores.Tokens
		keys := newAPIKeyIndex()
		if toks, err := stores.Tokens.List(); err != nil {
			log.Printf("Warning: failed to load API tokens from store: %v", err)
		} else {
			keys.load(toks)
			if len(toks) > 0 {
				log.Printf("Loaded %d issued API token(s) from store", len(toks))
			}
		}
		cfg.Keys = keys
	}
	handler := buildMux(cfg, stores, tunnelStore, directStore)

	// E2B-compatible gateway (opt-in). Two ways to enable:
	//   SANDRPOD_E2B_DOMAIN — production: host-routed on the main port
	//     (api.<domain> + <port>-<sandboxID>.<domain>); needs wildcard DNS/TLS.
	//   SANDRPOD_E2B_DEBUG_PORT — a plain-HTTP listener that serves ONLY the
	//     gateway in path mode, for pointing the unmodified E2B SDK at over
	//     http (E2B_API_URL + E2B_SANDBOX_URL = http://host:<port>,
	//     E2B_VALIDATE_API_KEY=false). No DNS/TLS required. See docs/E2B_COMPAT.md.
	e2bDepsVal := e2bDeps{
		cfg:         cfg,
		scheduler:   podpkg.NewScheduler(stores.Poders, cfg.APIURL, cfg.Token),
		sandboxes:   stores.Sandboxes,
		poders:      stores.Poders,
		jobs:        stores.Jobs,
		tunnelStore: tunnelStore,
		directStore: directStore,
	}
	if dom := os.Getenv("SANDRPOD_E2B_DOMAIN"); dom != "" {
		handler = e2bHostRouter(dom, newE2BGateway(dom, e2bDepsVal), handler)
		log.Printf("E2B-compatible gateway enabled: api.%s (control plane) + <port>-<id>.%s (envd)", dom, dom)
	}
	if dbg := os.Getenv("SANDRPOD_E2B_DEBUG_PORT"); dbg != "" {
		gw := newE2BGateway("", e2bDepsVal) // path mode, single-sandbox resolver
		if os.Getenv("SANDRPOD_E2B_DEBUG_LOG") != "" {
			inner := gw
			gw = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lw := &statusRecorder{ResponseWriter: w, status: 200}
				inner.ServeHTTP(lw, r)
				log.Printf("E2B %s %s?%s -> %d | apikey=%q auth=%q keys=%v", r.Method, r.URL.Path, r.URL.RawQuery, lw.status,
					r.Header.Get("X-API-KEY"), r.Header.Get("Authorization"), headerNames(r))
			})
		}
		// Bind the control-plane port plus the fixed envd (49983) and jupyter
		// (49999) ports so E2B_DEBUG=true clients — which target localhost:<port>
		// per get_host — reach the same gateway. Path routing (/sandboxes vs
		// /filesystem.* vs /execute) does the dispatch regardless of port.
		for _, addr := range []string{":" + dbg, ":49983", ":49999"} {
			addr := addr
			go func() {
				log.Printf("E2B-compatible gateway (HTTP debug) listening on %s", addr)
				if err := http.ListenAndServe(addr, gw); err != nil {
					log.Printf("E2B debug gateway %s error: %v", addr, err)
				}
			}()
		}
		log.Printf("E2B debug: set E2B_DEBUG=true + E2B_API_URL=http://localhost:%s (envd/jupyter auto on :49983/:49999)", dbg)
	}

	// Start the HTTP server
	addr := fmt.Sprintf(":%d", *port)
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// No global write deadline: this server holds long-lived responses —
		// tunnelled toolbox streams, PTY sessions, SSE, and the E2B drop-in's
		// synchronous Sandbox.create() which blocks minutes while a cloud VM
		// provisions. A 120s WriteTimeout silently truncated all of them (a
		// cold GCP create → 502 at the reverse proxy). Per-request bounds come
		// from ReadTimeout + each handler's own context (create uses a 20-min
		// detached context; proxies set their own deadlines).
		WriteTimeout: 0,
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
	// Idle reclamation (cost safety). The sandbox reaper always runs so
	// per-sandbox ttl_seconds work; the global default stays opt-in (0 = only
	// sandboxes that set their own TTL are reaped).
	if *sandboxIdleTTL > 0 {
		log.Printf("Idle-sandbox reaper enabled: default ttl=%v", *sandboxIdleTTL)
	}
	go reapIdleSandboxes(rootCtx, *sandboxIdleTTL, stores.Sandboxes, stores.Poders, tunnelStore)
	if *poderIdleTTL > 0 {
		log.Printf("Idle-poder reaper enabled: ttl=%v (empty cloud poders reclaimed, VMs terminated)", *poderIdleTTL)
		go reapIdlePoders(rootCtx, *poderIdleTTL, stores.Poders, stores.Sandboxes, tunnelStore)
	}

	if cfg.Registry != nil && *tokensFile != "" {
		go watchTokensFile(rootCtx, *tokensFile, cfg.Registry)
	}

	// Multi-instance: periodically re-sync the token index from the shared store
	// so tokens issued OR revoked on peer instances converge here (issuance is
	// also picked up instantly via the FindByHash fallback in resolveToken).
	if *nodeURL != "" && cfg.Keys != nil && stores.Tokens != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-ticker.C:
					if toks, err := stores.Tokens.List(); err == nil {
						cfg.Keys.replace(toks)
					}
				}
			}
		}()
	}

	if *tlsCert != "" && *tlsKey != "" {
		log.Printf("API server listening on %s (HTTPS, Control Plane + Tunnel Mode)", addr)
		if err := server.ListenAndServeTLS(*tlsCert, *tlsKey); err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
		return
	}
	log.Printf("API server listening on %s (Control Plane + Tunnel Mode)", addr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

// sandboxTunnel looks up a sandbox and its Poder tunnel together.
// Writes an appropriate HTTP error and returns false if either is missing.
// sandboxTunnel resolves a sandbox to the reverse tunnel that reaches its
// container (or direct agent), and records activity for the idle reaper — the
// single choke point every proxy path funnels through.
//
// Multi-instance: the yamux tunnel lives on exactly one instance. When it isn't
// on this one, the request r is forwarded to the peer node that owns it
// (owners.NodeFor); this returns ok=false with the response already written. A
// request already forwarded once (forwardedHeader) is not forwarded again, so a
// stale owner map degrades to a clean 503 instead of a loop.
func sandboxTunnel(name string, r *http.Request, ss podpkg.SandboxRepository, ts, ds *tunnel.TunnelStore, owners podpkg.TunnelOwnerRepository, nodeURL string, w http.ResponseWriter) (*podpkg.SandboxInfo, *tunnel.PoderTunnel, bool) {
	sb, ok := ss.Get(name)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return nil, nil, false
	}
	_ = ss.Update(name, func(s *podpkg.SandboxInfo) { s.LastActivity = time.Now() })

	// Pick the local tunnel store + ownership key by path: a direct-agent
	// sandbox is keyed by its own name, a poder-backed one by its poder id.
	key, local := sb.PoderID, ts
	if strings.HasPrefix(sb.ProxyURL, "direct://") {
		key, local = name, ds
	}
	if t, ok := local.Get(key); ok {
		return sb, t, true // tunnel is on this instance (the common case)
	}
	// Not local: forward to the peer instance holding it (multi-instance only).
	if nodeURL != "" && owners != nil && r != nil && r.Header.Get(forwardedHeader) == "" {
		if owner, found := owners.NodeFor(key); found && owner != nodeURL {
			forwardToNode(owner, w, r)
			return nil, nil, false // response written by the forward
		}
	}
	http.Error(w, "sandbox tunnel not available", http.StatusServiceUnavailable)
	return nil, nil, false
}

// forwardedHeader marks a request already forwarded once between instances, so
// the receiving node won't forward again (guards against a stale owner map).
const forwardedHeader = "X-Sandrpod-Forwarded"

// forwardToNode reverse-proxies r to the peer instance that holds the target
// poder's tunnel. The peer serves the same endpoint, finds the tunnel locally,
// and proxies through it. FlushInterval=-1 streams chunks immediately so exec
// stream / SSE stay real-time across the hop.
func forwardToNode(nodeURL string, w http.ResponseWriter, r *http.Request) {
	target, err := url.Parse(nodeURL)
	if err != nil {
		http.Error(w, "invalid owner node url", http.StatusBadGateway)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	base := proxy.Director
	proxy.Director = func(req *http.Request) {
		base(req)
		req.Header.Set(forwardedHeader, "1")
		req.Host = target.Host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, e error) {
		log.Printf("inter-node forward to %s failed: %v", nodeURL, e)
		http.Error(w, "owner node unreachable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
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

// flushCopy relays a streaming response body to w, flushing after every chunk
// so streamed output (SSE execution output, MCP event streams) reaches the
// client in real time instead of being re-buffered by a plain io.Copy.
func flushCopy(w http.ResponseWriter, r io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, readErr := r.Read(buf)
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
	flushCopy(w, resp.Body)
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
			reapOfflinePodersOnce(ctx, time.Now(), timeout, ps, factoryVMTerminator)
		}
	}
}

// reapOfflinePodersOnce runs one liveness sweep: it reclaims any poder that has
// been OFFLINE (no heartbeat) longer than timeout — terminating the cloud VM
// (only for cloud poders with a VM; local/docker poders have none), tombstoning
// the ID, and deleting the record. This is liveness cleanup of dead poders, not
// idle/cost reclamation: an ONLINE poder is never touched here, and a local
// poder loses only its stale record (there is no VM to terminate). Pure w.r.t.
// time and the cloud API so it is directly unit-testable.
func reapOfflinePodersOnce(ctx context.Context, now time.Time, timeout time.Duration, ps podpkg.PoderRepository, terminate vmTerminator) {
	for _, p := range ps.List() {
		if p.State != podpkg.PoderStateOffline || now.Sub(p.LastHeartbeat) <= timeout {
			continue
		}
		if isCloudProvider(p.ProviderType) && p.VMID != "" {
			if err := terminate(ctx, p.ProviderType, p.VMID); err != nil {
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
