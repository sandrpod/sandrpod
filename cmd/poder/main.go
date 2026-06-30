// Copyright 2024 SandrPod
// Combined Proxy + Poder Agent service.
// Deployed on every worker node to join the sandrpod network.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/sandrpod/sandrpod/pkg/poder"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

func env(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

var (
	apiURL            = flag.String("api-url", env("API_URL", "http://localhost:8080"), "API Server URL")
	apiToken          = flag.String("token", env("SANDRPOD_TOKEN", ""), "API Server bearer token")
	region            = flag.String("region", env("REGION", "local"), "Region")
	providerType      = flag.String("provider-type", env("PROVIDER_TYPE", "local"), "Provider type: aws, aliyun, local, docker")
	vmInstanceID      = flag.String("vm-instance-id", env("VM_INSTANCE_ID", ""), "Cloud VM instance ID this Poder runs on (used for VM reclamation)")
	poderIDFlag       = flag.String("poder-id", env("PODER_ID", ""), "Poder ID (auto-generated if not set)")
	networkName       = flag.String("network", env("SANDRPOD_NETWORK", ""), "Docker network name for sandbox containers (empty = Docker default bridge)")
	heartbeatInterval = flag.Duration("heartbeat-interval", 10*time.Second, "Heartbeat interval")
	help              = flag.Bool("help", false, "Show help")
)

func main() {
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	log.Printf("Starting SandrPod Poder v0.3.0")
	log.Printf("API Server: %s, Region: %s", *apiURL, *region)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := poder.NewDockerPoder(*region, *networkName)
	if err != nil {
		log.Fatalf("Failed to create Docker Poder: %v", err)
	}

	poderID, err := resolvePoderID(*poderIDFlag)
	if err != nil {
		log.Fatalf("Failed to resolve Poder ID: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": "0.3.0",
			"mode":    "tunnel",
		})
	})

	mux.HandleFunc("/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req poder.CreatePodRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		log.Printf("Creating sandbox: %s", req.Name)

		podInfo, err := p.CreatePod(ctx, &req)
		if err != nil {
			log.Printf("Failed to create sandbox: %v", err)
			http.Error(w, fmt.Sprintf("Failed to create sandbox: %v", err), http.StatusInternalServerError)
			return
		}

		if err := p.WaitUntilRunning(ctx, podInfo.ID, 5*time.Minute); err != nil {
			log.Printf("Failed to wait for sandbox running: %v", err)
			http.Error(w, fmt.Sprintf("Failed to wait for sandbox: %v", err), http.StatusInternalServerError)
			return
		}

		podInfo, _ = p.GetPod(ctx, podInfo.ID)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id":    podInfo.ID,
			"name":  podInfo.Name,
			"ip":    podInfo.IP,
			"state": podInfo.State,
		})
	})

	mux.HandleFunc("/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/sandboxes/"):]
		if path == "" {
			http.Error(w, "sandbox name is required", http.StatusBadRequest)
			return
		}

		parts := strings.SplitN(path, "/", 2)
		sandboxName := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}

		log.Printf("Sandbox operation: method=%s, name=%s, action=%s", r.Method, sandboxName, action)

		switch r.Method {
		case http.MethodGet:
			pod, err := p.FindPodByName(ctx, sandboxName)
			if err != nil {
				http.Error(w, fmt.Sprintf("Sandbox %s not found", sandboxName), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":    pod.ID,
				"name":  pod.Name,
				"ip":    pod.IP,
				"state": pod.State,
			})

		case http.MethodPost:
			if action == "start" {
				pod, err := p.FindPodByName(ctx, sandboxName)
				if err != nil {
					http.Error(w, fmt.Sprintf("Sandbox %s not found", sandboxName), http.StatusNotFound)
					return
				}
				if pod.State == poder.PodStateStopped {
					if err := p.UnpausePod(ctx, pod.ID); err != nil {
						http.Error(w, fmt.Sprintf("Failed to start sandbox: %v", err), http.StatusInternalServerError)
						return
					}
				}
				if err := p.WaitUntilRunning(ctx, pod.ID, 5*time.Minute); err != nil {
					http.Error(w, fmt.Sprintf("Failed to wait for sandbox: %v", err), http.StatusInternalServerError)
					return
				}
				pod, _ = p.GetPod(ctx, pod.ID)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"id":    pod.ID,
					"name":  pod.Name,
					"ip":    pod.IP,
					"state": pod.State,
				})
				return
			}
			if action == "stop" {
				pod, err := p.FindPodByName(ctx, sandboxName)
				if err != nil {
					http.Error(w, fmt.Sprintf("Sandbox %s not found", sandboxName), http.StatusNotFound)
					return
				}
				if err := p.PausePod(ctx, pod.ID); err != nil {
					http.Error(w, fmt.Sprintf("Failed to stop sandbox: %v", err), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"id":    pod.ID,
					"name":  pod.Name,
					"ip":    pod.IP,
					"state": poder.PodStateStopped,
				})
				return
			}
			http.Error(w, "Action not allowed", http.StatusMethodNotAllowed)

		case http.MethodDelete:
			pod, err := p.FindPodByName(ctx, sandboxName)
			if err != nil {
				http.Error(w, fmt.Sprintf("Sandbox %s not found", sandboxName), http.StatusNotFound)
				return
			}
			if err := p.DeletePod(ctx, pod.ID); err != nil {
				http.Error(w, fmt.Sprintf("Failed to delete sandbox: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})

		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/execute", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sandboxName := r.URL.Query().Get("sandbox")
		if sandboxName == "" {
			http.Error(w, "sandbox name is required", http.StatusBadRequest)
			return
		}
		pod, err := p.FindPodByName(ctx, sandboxName)
		if err != nil {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", sandboxName), http.StatusNotFound)
			return
		}
		if pod.IP == "" {
			http.Error(w, "Sandbox IP not available", http.StatusInternalServerError)
			return
		}
		toolboxURL := fmt.Sprintf("http://%s:8080/process", pod.IP)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, toolboxURL, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to call toolbox: %v", err)
			http.Error(w, "Failed to execute code", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sandboxName := r.URL.Query().Get("sandbox")
		if sandboxName == "" {
			http.Error(w, "sandbox name is required", http.StatusBadRequest)
			return
		}
		pod, err := p.FindPodByName(r.Context(), sandboxName)
		if err != nil {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", sandboxName), http.StatusNotFound)
			return
		}
		toolboxURL := fmt.Sprintf("http://%s:8080/stream", pod.IP)
		if r.URL.RawQuery != "" {
			toolboxURL += "?" + r.URL.RawQuery
		}
		var toolboxReq *http.Request
		if r.Method == http.MethodPost {
			toolboxReq, err = http.NewRequestWithContext(r.Context(), http.MethodPost, toolboxURL, r.Body)
		} else {
			toolboxReq, err = http.NewRequestWithContext(r.Context(), http.MethodGet, toolboxURL, nil)
		}
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}
		for k, v := range r.Header {
			if k != "Host" {
				toolboxReq.Header[k] = v
			}
		}
		client := &http.Client{Timeout: 0}
		resp, err := client.Do(toolboxReq)
		if err != nil {
			log.Printf("Failed to call toolbox stream: %v", err)
			http.Error(w, "Failed to execute code", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		sandboxName := r.URL.Query().Get("sandbox")
		if sandboxName == "" {
			http.Error(w, "sandbox name is required", http.StatusBadRequest)
			return
		}
		tail := r.URL.Query().Get("tail")
		pod, err := p.FindPodByName(r.Context(), sandboxName)
		if err != nil {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", sandboxName), http.StatusNotFound)
			return
		}
		logs, err := p.GetPodLogs(r.Context(), pod.ID, tail)
		if err != nil {
			log.Printf("Failed to get pod logs: %v", err)
			http.Error(w, "Failed to get logs", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(logs))
	})

	mux.HandleFunc("/process/session/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/process/session/"):]
		if path == "" {
			http.Error(w, "sandbox name is required", http.StatusBadRequest)
			return
		}
		parts := bytes.Split([]byte(path), []byte("/"))
		sandboxName := string(parts[0])
		var toolboxPath string
		if len(parts) >= 2 {
			toolboxPath = "/" + string(bytes.Join(parts[1:], []byte("/")))
		}
		log.Printf("Session proxy for sandbox: %s, path: %s", sandboxName, toolboxPath)
		pod, err := p.FindPodByName(r.Context(), sandboxName)
		if err != nil {
			http.Error(w, "Sandbox not found", http.StatusNotFound)
			return
		}
		if pod.IP == "" {
			http.Error(w, "Sandbox IP not available", http.StatusInternalServerError)
			return
		}
		var toolboxURL string
		if toolboxPath != "" {
			if r.URL.RawQuery != "" {
				toolboxURL = fmt.Sprintf("http://%s:8080/process/session%s?%s", pod.IP, toolboxPath, r.URL.RawQuery)
			} else {
				toolboxURL = fmt.Sprintf("http://%s:8080/process/session%s", pod.IP, toolboxPath)
			}
		} else {
			if r.URL.RawQuery != "" {
				toolboxURL = fmt.Sprintf("http://%s:8080/process/session?%s", pod.IP, r.URL.RawQuery)
			} else {
				toolboxURL = fmt.Sprintf("http://%s:8080/process/session", pod.IP)
			}
		}
		log.Printf("Proxying to: %s %s", r.Method, toolboxURL)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, toolboxURL, r.Body)
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}
		for k, v := range r.Header {
			req.Header[k] = v
		}
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to call toolbox session: %v", err)
			http.Error(w, "Failed to proxy request", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	mux.HandleFunc("/toolbox/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/toolbox/"):]
		if path == "" {
			http.Error(w, "sandbox name is required", http.StatusBadRequest)
			return
		}
		parts := bytes.Split([]byte(path), []byte("/"))
		if len(parts) < 2 {
			http.Error(w, "invalid path, expected /toolbox/{sandboxName}/*", http.StatusBadRequest)
			return
		}
		sandboxName := string(parts[0])
		toolboxPath := string(bytes.Join(parts[1:], []byte("/")))
		log.Printf("Toolbox proxy for sandbox: %s, path: %s", sandboxName, toolboxPath)
		pod, err := p.FindPodByName(r.Context(), sandboxName)
		if err != nil {
			http.Error(w, "Sandbox not found", http.StatusNotFound)
			return
		}
		if pod.IP == "" {
			http.Error(w, "Sandbox IP not available", http.StatusInternalServerError)
			return
		}
		targetPath := toolboxPath
		if r.Method == http.MethodDelete && toolboxPath == "files" {
			targetPath = "files/delete"
		}
		var toolboxURL string
		if r.URL.RawQuery != "" {
			toolboxURL = fmt.Sprintf("http://%s:8080/%s?%s", pod.IP, targetPath, r.URL.RawQuery)
		} else {
			toolboxURL = fmt.Sprintf("http://%s:8080/%s", pod.IP, targetPath)
		}
		log.Printf("Proxying to: %s %s", r.Method, toolboxURL)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, toolboxURL, r.Body)
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}
		for k, v := range r.Header {
			req.Header[k] = v
		}
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to call toolbox: %v", err)
			http.Error(w, "Failed to proxy request", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	mux.HandleFunc("/pty/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("PTY handler called with path: %s", r.URL.Path)
		path := r.URL.Path[len("/pty/"):]
		if path == "" {
			http.Error(w, "sandbox name is required", http.StatusBadRequest)
			return
		}
		parts := bytes.Split([]byte(path), []byte("/connect"))
		if len(parts) < 2 {
			http.Error(w, "invalid path, expected /pty/{sandboxName}/connect", http.StatusBadRequest)
			return
		}
		sandboxName := string(parts[0])
		log.Printf("PTY handler for sandbox: %s", sandboxName)
		pod, err := p.FindPodByName(r.Context(), sandboxName)
		if err != nil {
			http.Error(w, "Sandbox not found", http.StatusNotFound)
			return
		}
		if pod.IP == "" {
			http.Error(w, "Sandbox IP not available", http.StatusInternalServerError)
			return
		}
		createURL := fmt.Sprintf("http://%s:8080/pty/create?width=80&height=24", pod.IP)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, createURL, nil)
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to create PTY session: %v", err)
			http.Error(w, "Failed to create PTY session", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("PTY create failed: %s", string(body))
			http.Error(w, "Failed to create PTY session", http.StatusInternalServerError)
			return
		}
		var createResp struct {
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
			http.Error(w, "Invalid response from Toolbox", http.StatusInternalServerError)
			return
		}
		sessionID := createResp.SessionID
		log.Printf("Created PTY session: %s for sandbox: %s", sessionID, sandboxName)
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		toolboxWSURL := fmt.Sprintf("ws://%s:8080/pty/%s", pod.IP, sessionID)
		toolboxConn, _, err := websocket.DefaultDialer.Dial(toolboxWSURL, nil)
		if err != nil {
			log.Printf("Failed to connect to Toolbox WebSocket: %v", err)
			http.Error(w, "Failed to connect to PTY session", http.StatusInternalServerError)
			return
		}
		defer toolboxConn.Close()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		log.Printf("PTY WebSocket connected for session: %s", sessionID)
		done := make(chan struct{})
		closeOnce := sync.Once{}
		go func() {
			for {
				_, message, err := toolboxConn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						log.Printf("Toolbox WebSocket read error: %v", err)
					}
					closeOnce.Do(func() { close(done) })
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
					log.Printf("Client write error: %v", err)
					closeOnce.Do(func() { close(done) })
					return
				}
			}
		}()
		go func() {
			for {
				msgType, message, err := conn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						log.Printf("Client read error: %v", err)
					}
					closeOnce.Do(func() { close(done) })
					return
				}
				if msgType == websocket.CloseMessage {
					closeOnce.Do(func() { close(done) })
					return
				}
				if err := toolboxConn.WriteMessage(websocket.BinaryMessage, message); err != nil {
					log.Printf("Toolbox write error: %v", err)
					closeOnce.Do(func() { close(done) })
					return
				}
			}
		}()
		<-done
		log.Printf("PTY WebSocket session ended: %s", sessionID)
	})

	go connectTunnel(ctx, poderID, mux, p)
	go heartbeatLoop(ctx, poderID, p)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down Poder...")
	cancel()
}

// connectTunnel dials API Server via WebSocket, establishes a yamux session,
// and serves the existing mux over yamux streams. Reconnects on disconnect.
func connectTunnel(ctx context.Context, poderID string, mux http.Handler, p *poder.DockerPoder) {
	wsURL := *apiURL
	switch {
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + wsURL[len("https://"):]
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + wsURL[len("http://"):]
	}
	wsURL += "/ws/poder/connect"

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		resources, err := getHostResources()
		if err != nil {
			resources = podpkg.PoderResources{
				CPUCores:      4,
				MemoryBytes:   8 * 1024 * 1024 * 1024,
				MaxContainers: 10,
			}
		}

		headers := http.Header{}
		if *apiToken != "" {
			headers.Set("Authorization", "Bearer "+*apiToken)
		}
		headers.Set("X-Poder-ID", poderID)
		headers.Set("X-Poder-Name", p.Name())
		headers.Set("X-Poder-Region", *region)
		headers.Set("X-Poder-Provider-Type", *providerType)
		headers.Set("X-Poder-VM-ID", *vmInstanceID)
		headers.Set("X-Poder-CPU-Cores", strconv.Itoa(resources.CPUCores))
		headers.Set("X-Poder-Memory-Bytes", strconv.FormatInt(resources.MemoryBytes, 10))
		headers.Set("X-Poder-Max-Containers", strconv.Itoa(resources.MaxContainers))
		headers.Set("X-Poder-Arch", resources.Arch)
		headers.Set("X-Poder-OS", resources.OS)
		headers.Set("X-Poder-OS-Version", resources.OSVersion)
		headers.Set("X-Poder-Kernel-Version", resources.KernelVersion)

		wsConn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, headers)
		if err != nil {
			log.Printf("Tunnel dial failed: %v (retrying in 5s)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		log.Printf("Tunnel connected to API Server as %s", poderID)

		cfg := yamux.DefaultConfig()
		session, err := yamux.Server(tunnel.NewWSConn(wsConn), cfg)
		if err != nil {
			log.Printf("yamux.Server failed: %v", err)
			wsConn.Close()
			continue
		}

		httpSrv := &http.Server{Handler: mux}
		serveDone := make(chan struct{})
		go func() {
			defer close(serveDone)
			if err := httpSrv.Serve(session); err != nil && err != http.ErrServerClosed {
				log.Printf("yamux HTTP serve ended: %v", err)
			}
		}()

		select {
		case <-ctx.Done():
			httpSrv.Close()
			return
		case <-serveDone:
		}
		httpSrv.Close()
		log.Printf("Tunnel disconnected, reconnecting in 3s...")
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// heartbeatLoop sends periodic heartbeats to API Server via HTTP.
func heartbeatLoop(ctx context.Context, poderID string, p *poder.DockerPoder) {
	ticker := time.NewTicker(*heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Query Docker directly for the authoritative list of running containers.
			// This is correct even after a Poder restart (BasePoder.ListPods only
			// knows about containers created in the current process lifetime).
			containerNames, err := p.ListRunningSandboxNames(ctx)
			if err != nil {
				log.Printf("Heartbeat: ListRunningSandboxNames failed: %v", err)
				containerNames = nil // send nil → server skips reconcile this tick
			}
			containerCount := len(containerNames)
			cpuUsage, memUsage := getHostUsage()

			url := fmt.Sprintf("%s/api/v1/poders/%s/heartbeat", *apiURL, poderID)
			reqBody := podpkg.HeartbeatRequest{
				Containers:     containerCount,
				CPUUsage:       cpuUsage,
				MemoryUsage:    memUsage,
				ContainerNames: containerNames,
			}
			body, _ := json.Marshal(reqBody)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			if *apiToken != "" {
				req.Header.Set("Authorization", "Bearer "+*apiToken)
			}
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("Heartbeat failed: %v", err)
				continue
			}
			resp.Body.Close()
		}
	}
}

func getHostResources() (podpkg.PoderResources, error) {
	cpuCount := 0
	data, err := os.ReadFile("/proc/cpuinfo")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "processor") {
				cpuCount++
			}
		}
	}
	if cpuCount == 0 {
		cpuCount = 4
	}

	memTotal := int64(0)
	memData, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(memData), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					memTotal, _ = strconv.ParseInt(fields[1], 10, 64)
					memTotal *= 1024
				}
				break
			}
		}
	}
	if memTotal == 0 {
		memTotal = 8 * 1024 * 1024 * 1024
	}

	osVersion := getOSVersion()
	kernelVersion := getKernelVersion()

	return podpkg.PoderResources{
		CPUCores:      cpuCount,
		MemoryBytes:   memTotal,
		MaxContainers: 10,
		Arch:          runtime.GOARCH,
		OS:            runtime.GOOS,
		OSVersion:     osVersion,
		KernelVersion: kernelVersion,
	}, nil
}

// getOSVersion reads /etc/os-release for a human-readable OS version string.
// Falls back to runtime.GOOS on failure.
func getOSVersion() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			val := strings.TrimPrefix(line, "PRETTY_NAME=")
			return strings.Trim(val, `"`)
		}
	}
	return runtime.GOOS
}

// getKernelVersion reads /proc/version for the kernel version string.
func getKernelVersion() string {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return ""
	}
	// /proc/version is one line; extract up to "version X.Y.Z"
	line := strings.TrimSpace(string(data))
	fields := strings.Fields(line)
	// Format: "Linux version 5.15.0-91-generic ..."
	if len(fields) >= 3 && strings.EqualFold(fields[0], "linux") {
		return fields[2]
	}
	return line
}

func getHostUsage() (cpuUsage, memUsage float64) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Dial: func(proto, addr string) (net.Conn, error) {
				return net.Dial("unix", "/var/run/docker.sock")
			},
		},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost/v1.24/info", nil)
	if err != nil {
		return 0.5, 0.5
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0.5, 0.5
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0.5, 0.5
	}
	return getCPUUsage(), getMemoryUsage()
}

func getCPUUsage() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0.5
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				return 0.5
			}
			var total, idle int64
			for i := 1; i < len(fields); i++ {
				v, _ := strconv.ParseInt(fields[i], 10, 64)
				total += v
				if i == 4 {
					idle = v
				}
			}
			if total == 0 {
				return 0.5
			}
			return float64(total-idle) / float64(total)
		}
	}
	return 0.5
}

func getMemoryUsage() float64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0.5
	}
	var memTotal, memAvailable int64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				memTotal, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				memAvailable, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
	}
	if memTotal == 0 {
		return 0.5
	}
	return float64(memTotal-memAvailable) / float64(memTotal)
}

// resolvePoderID returns a stable Poder ID across restarts.
//
// Priority:
//  1. Explicit flag / env PODER_ID  — always wins, no persistence needed
//  2. Persisted ID in $PODER_DATA_DIR/poder-id (default: /var/lib/sandrpod/poder-id)
//  3. Auto-generate a random ID, persist it for next boot
//
// This ensures the Poder keeps the same ID after container restarts so that
// the API Server can reassociate existing sandbox records to the reconnected Poder.
func resolvePoderID(explicit string) (string, error) {
	if explicit != "" {
		log.Printf("Poder ID (explicit): %s", explicit)
		return explicit, nil
	}

	dataDir := env("PODER_DATA_DIR", "/var/lib/sandrpod")
	idFile := filepath.Join(dataDir, "poder-id")

	// Try reading persisted ID.
	if data, err := os.ReadFile(idFile); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			log.Printf("Poder ID (persisted): %s", id)
			return id, nil
		}
	}

	// Generate a new random ID.
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random poder id: %w", err)
	}
	id := "poder-" + hex.EncodeToString(buf)

	// Persist it so subsequent restarts use the same ID.
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		// Non-fatal: log and continue without persistence.
		log.Printf("Warning: cannot create data dir %s: %v — ID will not be persisted", dataDir, err)
	} else if err := os.WriteFile(idFile, []byte(id+"\n"), 0o644); err != nil {
		log.Printf("Warning: cannot write poder-id file %s: %v — ID will not be persisted", idFile, err)
	} else {
		log.Printf("Poder ID (generated, persisted to %s): %s", idFile, id)
	}

	return id, nil
}
