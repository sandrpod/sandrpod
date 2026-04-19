// Copyright 2024 SandrPod
// API Server - REST API 控制面服务
// 只负责接收请求、创建任务，不直接连接云厂商

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
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
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	sqlitestore "github.com/sandrpod/sandrpod/pkg/store/sqlite"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

var (
	port           = flag.Int("port", 8080, "API server port")
	help           = flag.Bool("help", false, "Show help")
	token          = flag.String("token", "", "API token for authentication")
	offlineTimeout = flag.Duration("offline-timeout", 30*time.Second, "Poder offline timeout")
	dbDSN          = flag.String("db", "", `persistence backend: empty=in-memory (default), sqlite:<path>=SQLite file (e.g. sqlite:./data/sandrpod.db)`)
)

func main() {
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	log.Printf("Starting SandrPod API Server v0.3.0 (Control Plane)")

	// 注册云 Provider
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

	factory := provider.GetFactory()
	log.Printf("Registered providers: %v", factory.Names())

	// ── Store construction ────────────────────────────────────────────────────
	var stores podpkg.Stores
	switch {
	case *dbDSN == "":
		stores = store.NewMemoryStores()
		log.Printf("Using in-memory store (data will be lost on restart)")
	case strings.HasPrefix(*dbDSN, "sqlite:"):
		dsn := strings.TrimPrefix(*dbDSN, "sqlite:")
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

	jobStore := stores.Jobs
	sandboxStore := stores.Sandboxes
	poderStore := stores.Poders
	tunnelStore := tunnel.NewTunnelStore()
	directStore := tunnel.NewDirectTunnelStore() // direct sandbox agent tunnels (toC)
	scheduler := podpkg.NewScheduler(poderStore, fmt.Sprintf("http://localhost:%d", *port))

	mux := http.NewServeMux()

	wsUpgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// 认证中间件
	authMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if *token != "" {
				if r.Header.Get("Authorization") != "Bearer "+*token {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next(w, r)
		}
	}

	// 健康检查
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "ok",
			"version":   "0.3.0",
			"timestamp": time.Now().Unix(),
			"mode":      "control-plane+tunnel",
		})
	})

	// === Poder 隧道入口 ===
	// Poder 启动时拨入此端点，完成注册并建立 yamux 反向隧道
	mux.HandleFunc("/ws/poder/connect", func(w http.ResponseWriter, r *http.Request) {
		poderID := r.Header.Get("X-Poder-ID")
		if poderID == "" {
			http.Error(w, "X-Poder-ID header required", http.StatusBadRequest)
			return
		}

		ws, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Poder %s WebSocket upgrade failed: %v", poderID, err)
			return
		}

		// 从 headers 解析注册信息
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
			URL:          "tunnel://" + poderID, // 标记为 tunnel 模式，不用于直接 HTTP
			Region:       r.Header.Get("X-Poder-Region"),
			ProviderType: r.Header.Get("X-Poder-Provider-Type"),
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

		// 断线清理：仅当 store 中存的还是本次 tunnel（未被新连接覆盖）时才清理
		defer func() {
			if cur, ok := tunnelStore.Get(poderID); ok && cur == t {
				tunnelStore.Remove(poderID)
				poderStore.SetOffline(poderID)
			}
			t.Close()
			log.Printf("Poder %s tunnel disconnected", poderID)
		}()

		// 阻塞直到 yamux 连接断开（内部每 3s Ping 一次，失败即返回）
		t.Wait()
	})

	// === 本地 Agent 直连入口（toC 场景）===
	// sandrpod-agent 启动时拨入此端点，把本机注册为一个直连 Sandbox
	mux.HandleFunc("/ws/sandbox/connect", func(w http.ResponseWriter, r *http.Request) {
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

		// 建立 yamux 隧道（API Server 作为 yamux client，Agent 作为 server）
		t, err := tunnel.NewPoderTunnel(sandboxName, ws)
		if err != nil {
			log.Printf("Agent %s tunnel creation failed: %v", sandboxName, err)
			ws.Close()
			return
		}
		directStore.Set(sandboxName, t)

		// 注册/更新 Sandbox 元数据
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
		// 已存在则更新（agent 重连场景），不存在则新增
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
	})

	// === Poder API ===
	// GET /api/v1/poders - 列出所有 Poder
	mux.HandleFunc("/api/v1/poders", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		poders := poderStore.List()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"poders": poders})
	}))

	// /api/v1/poders/* - Poder 详情 + heartbeat + 直接 sandbox 操作
	mux.HandleFunc("/api/v1/poders/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/api/v1/poders/"):]

		// DELETE /api/v1/poders/{id} — 删除 Poder 记录
		if !strings.Contains(path, "/") && r.Method == http.MethodDelete {
			pID := path
			if _, ok := poderStore.Get(pID); !ok {
				http.Error(w, "poder not found", http.StatusNotFound)
				return
			}
			// 强制断开 tunnel（若仍在线）
			if t, ok := tunnelStore.Get(pID); ok {
				t.Close()
			}
			if err := poderStore.Delete(pID); err != nil {
				http.Error(w, fmt.Sprintf("failed to delete poder: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}

		// POST /api/v1/poders/{id}/heartbeat（向后兼容，tunnel 模式下仍可用）
		if strings.HasSuffix(path, "/heartbeat") && r.Method == http.MethodPost {
			pID := strings.TrimSuffix(path, "/heartbeat")
			var req podpkg.HeartbeatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := poderStore.Heartbeat(pID, &req); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}

		// /api/v1/poders/{id}/sandboxes 系列
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

			// POST /api/v1/poders/{id}/sandboxes - 创建 sandbox（直接指定 Poder）
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
				if pi, ok := poderStore.Get(pID); ok {
					poderArch = pi.Resources.Arch
					poderOS = pi.Resources.OS
					poderOSVersion = pi.Resources.OSVersion
				}
				sandbox := &podpkg.SandboxInfo{
					ID:           poderResp.ID,
					Name:         req.Name,
					Region:       req.Region,
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

			// 解析 sandbox name 和 action
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

		// GET /api/v1/poders/{id}
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
			json.NewEncoder(w).Encode(map[string]interface{}{"sandboxes": sandboxes})

		case http.MethodPost:
			var req podpkg.CreateSandboxRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if req.ProviderType == "" {
				req.ProviderType = "local"
			}

			job, err := scheduler.ScheduleSandboxCreation(r.Context(), &req)
			if err != nil {
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

			// 通过隧道直接调用 Poder 创建 sandbox
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
			createReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://poder/sandboxes", bytes.NewReader(bodyBytes))
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

			var poderResp map[string]interface{}
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
			json.NewEncoder(w).Encode(map[string]interface{}{
				"job_id":  job.ID,
				"status":  "created",
				"sandbox": sandbox,
			})

		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// /api/v1/sandboxes/* - 单个 sandbox 操作 + 代理
	mux.HandleFunc("/api/v1/sandboxes/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/api/v1/sandboxes/"):]
		if path == "" {
			http.NotFound(w, r)
			return
		}

		// POST /api/v1/sandboxes/execute
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

		// GET|POST /api/v1/sandboxes/stream
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
			for k, v := range resp.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
			return
		}

		// 解析 {name} 和 action
		parts := splitPath(path)
		name := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}

		// session 路由检测
		sessionPath := ""
		sessionPrefix := name + "/session"
		if strings.HasPrefix(path, sessionPrefix) {
			sessionPath = strings.TrimPrefix(path, sessionPrefix)
			if sessionPath == "" && path == sessionPrefix {
				sessionPath = "/"
			}
		}

		log.Printf("[DEBUG] path=%q name=%q action=%q sessionPath=%q", path, name, action, sessionPath)

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

			// 返回 sandbox 信息
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
				for k, v := range resp.Header {
					w.Header()[k] = v
				}
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

			if action == "pty" {
				sb, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				_ = sb
				conn, err := wsUpgrader.Upgrade(w, r, nil)
				if err != nil {
					log.Printf("WebSocket upgrade failed: %v", err)
					return
				}
				defer conn.Close()
				workerConn, _, err := t.WSDialer.Dial("ws://poder/pty/"+name+"/connect", nil)
				if err != nil {
					log.Printf("Failed to connect to worker PTY: %v", err)
					conn.WriteMessage(websocket.TextMessage, []byte("Failed to connect to PTY session"))
					return
				}
				defer workerConn.Close()
				proxyWS(conn, workerConn)
				return
			}

			if action == "start" {
				sb, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
				if !ok {
					return
				}
				if strings.HasPrefix(sb.ProxyURL, "direct://") {
					// 本地 Agent 始终运行中，start 为 no-op
					sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateRunning })
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{"status": "running"})
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
					// 本地 Agent 无法暂停，stop 为 no-op
					sandboxStore.Update(name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateStopped })
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{"status": "stopped"})
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

			sb, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
			if !ok {
				return
			}
			if strings.HasPrefix(sb.ProxyURL, "direct://") {
				// 本地 Agent 直连沙箱：仅从 store 中移除记录
				sandboxStore.Delete(name)
				directStore.Remove(name)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "deleted"})
				return
			}
			if err := proxyHTTPErr(t, r, "http://poder/sandboxes/"+name, w); err == nil {
				pID := sb.PoderID
				sandboxStore.Delete(name)
				poderStore.UpdateUsage(pID, func(u *podpkg.PoderUsage) {
					if u.Containers > 0 {
						u.Containers--
					}
				})
			}
		}
	}))

	// === Job API (供 Poder Agent 调用) ===
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
		json.NewEncoder(w).Encode(map[string]interface{}{"jobs": jobs})
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

		var req podpkg.UpdateJobStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		err := jobStore.UpdateJob(jobID, func(job *podpkg.Job) {
			job.Status = req.Status
			if req.ErrorMessage != "" {
				job.ErrorMessage = req.ErrorMessage
			}
			if req.Result != nil {
				job.Result = req.Result
			}
			if req.Status == podpkg.JobStatusCompleted || req.Status == podpkg.JobStatusFailed {
				if sb, ok := sandboxStore.Get(job.SandboxName); ok {
					switch job.Type {
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
				}
			}
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
	}))

	// 启动服务器
	addr := fmt.Sprintf(":%d", *port)
	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	go cleanupOfflinePoders(poderStore, *offlineTimeout)

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
		http.NotFound(w, nil)
		w.WriteHeader(http.StatusNotFound)
		return nil, nil, false
	}
	// 直连 Agent 路径（toC）：sandbox 由本机 sandrpod-agent 直接注册
	if strings.HasPrefix(sb.ProxyURL, "direct://") {
		t, ok := ds.Get(name)
		if !ok {
			http.Error(w, "sandbox agent not connected", http.StatusServiceUnavailable)
			return nil, nil, false
		}
		return sb, t, true
	}
	// Poder 路径（toB）：sandbox 跑在 Poder 管理的容器里
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
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
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
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
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

// cleanupOfflinePoders marks Poder nodes as offline when heartbeats stop.
// Serves as a safety net alongside the tunnel disconnect handler.
func cleanupOfflinePoders(ps podpkg.PoderRepository, timeout time.Duration) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		<-ticker.C
		now := time.Now()
		for _, p := range ps.List() {
			if p.State == podpkg.PoderStateOnline && now.Sub(p.LastHeartbeat) > timeout {
				ps.SetOffline(p.ID)
				log.Printf("Poder %s marked OFFLINE (no heartbeat for %v)", p.ID, now.Sub(p.LastHeartbeat))
			}
		}
	}
}
