// Copyright 2024 SandrPod
// sandrpod-agent — toC 本地机器沙箱代理
//
// 将本机直接注册为 SandrPod Sandbox，无需 Poder/Docker 层。
// 通过 WebSocket + yamux 反向隧道连接 API Server，
// 嵌入 Toolbox 为远端 AI 提供代码执行能力。
//
// 使用方式：
//
//	sandrpod-agent -api-url=https://your-api-server.com -name=my-laptop -token=<api-key>
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/sandrpod/sandrpod/pkg/toolbox"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

func envOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

var (
	apiURL    = flag.String("api-url", envOr("SANDRPOD_API_URL", "http://localhost:8080"), "API Server URL")
	name      = flag.String("name", envOr("SANDRPOD_SANDBOX_NAME", ""), "Sandbox name (required, must be unique per API server)")
	token     = flag.String("token", envOr("SANDRPOD_TOKEN", ""), "API token for authentication")
	workDir   = flag.String("work-dir", envOr("SANDRPOD_WORK_DIR", ""), "Working directory for code execution (default: current dir)")
	reconnect = flag.Duration("reconnect", 5*time.Second, "Reconnect delay on disconnect")
	help      = flag.Bool("help", false, "Show help")
)

func main() {
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	if *name == "" {
		fmt.Fprintln(os.Stderr, "Error: -name is required")
		flag.Usage()
		os.Exit(1)
	}

	if *workDir != "" {
		if err := os.Chdir(*workDir); err != nil {
			log.Fatalf("Failed to change to work dir %s: %v", *workDir, err)
		}
	}

	log.Printf("Starting SandrPod Agent")
	log.Printf("Sandbox name: %s", *name)
	log.Printf("API Server:   %s", *apiURL)

	ctx, cancel := context.WithCancel(context.Background())

	// Build the handler once and reuse across reconnects.
	tb := toolbox.NewServer("")
	tbHandler := tb.Handler()
	agentHandler := newAgentMux(tbHandler, *name)

	go connectLoop(ctx, agentHandler)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down...")
	cancel()
}

// newAgentMux wraps the Toolbox handler with poder-protocol translation.
//
// The API Server always uses poder's URL schema when proxying to workers.
// This mux translates those paths to the toolbox-native paths:
//
//	POST /execute?sandbox=<name>              → POST /process
//	*    /toolbox/<name>/<subPath>            → * /<subPath>
//	*    /process/session/<name>              → * /process/session
//	*    /process/session/<name>/<rest>       → * /process/session/<rest>
//	GET  /logs?sandbox=<name>                → empty log response
//	*    everything else (/stream, /pty/, …) → toolbox directly (same paths)
func newAgentMux(tb http.Handler, sandboxName string) http.Handler {
	mux := http.NewServeMux()

	// /execute → /process
	mux.HandleFunc("/execute", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/process"
		tb.ServeHTTP(w, r2)
	})

	// /logs → empty (agent is a local process, no container logs)
	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"logs": ""})
	})

	// /toolbox/<name>/<subPath> → /<subPath>
	toolboxPrefix := "/toolbox/" + sandboxName + "/"
	mux.HandleFunc("/toolbox/", func(w http.ResponseWriter, r *http.Request) {
		sub, found := strings.CutPrefix(r.URL.Path, toolboxPrefix)
		if !found {
			// fallback: strip /toolbox/<anything>/
			rest := strings.TrimPrefix(r.URL.Path, "/toolbox/")
			if idx := strings.Index(rest, "/"); idx >= 0 {
				sub = rest[idx+1:]
			} else {
				http.Error(w, "invalid toolbox path", http.StatusBadRequest)
				return
			}
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + sub
		tb.ServeHTTP(w, r2)
	})

	// /process/session/<name>          → /process/session
	// /process/session/<name>/<rest>   → /process/session/<rest>
	//
	// The API Server prefixes the sandbox name into session paths so that
	// Poder nodes (which serve multiple sandboxes) can route correctly.
	// The agent serves exactly one sandbox, so strip the name prefix.
	sessionWithName := "/process/session/" + sandboxName
	mux.HandleFunc("/process/session/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		var toolboxPath string
		switch {
		case path == sessionWithName || path == sessionWithName+"/":
			toolboxPath = "/process/session"
		case strings.HasPrefix(path, sessionWithName+"/"):
			// /process/session/<name>/<rest> → /process/session/<rest>
			toolboxPath = "/process/session/" + strings.TrimPrefix(path, sessionWithName+"/")
		default:
			// path doesn't carry our sandbox name — pass through unchanged
			toolboxPath = path
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = toolboxPath
		tb.ServeHTTP(w, r2)
	})

	// Everything else (/stream, /pty/, /health, /info, …) → toolbox directly.
	mux.Handle("/", tb)

	return mux
}

// connectLoop dials the API Server and serves agentHandler over the yamux tunnel.
// Reconnects automatically on disconnect.
func connectLoop(ctx context.Context, handler http.Handler) {
	wsURL := toWS(*apiURL) + "/ws/sandbox/connect"

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		headers := buildHeaders()
		log.Printf("Connecting to %s as sandbox %q...", *apiURL, *name)

		wsConn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, headers)
		if err != nil {
			statusCode := 0
			if resp != nil {
				statusCode = resp.StatusCode
			}
			log.Printf("Connect failed (HTTP %d): %v — retry in %s", statusCode, err, *reconnect)
			select {
			case <-ctx.Done():
				return
			case <-time.After(*reconnect):
			}
			continue
		}
		log.Printf("Connected — sandbox %q is online", *name)

		cfg := yamux.DefaultConfig()
		cfg.KeepAliveInterval = 30 * time.Second
		session, err := yamux.Server(tunnel.NewWSConn(wsConn), cfg)
		if err != nil {
			log.Printf("yamux.Server failed: %v", err)
			wsConn.Close()
			continue
		}

		httpSrv := &http.Server{Handler: handler}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := httpSrv.Serve(session); err != nil && err != http.ErrServerClosed {
				log.Printf("Tunnel serve ended: %v", err)
			}
		}()

		select {
		case <-ctx.Done():
			httpSrv.Close()
			return
		case <-done:
		}
		httpSrv.Close()
		log.Printf("Disconnected — retry in %s", *reconnect)
		select {
		case <-ctx.Done():
			return
		case <-time.After(*reconnect):
		}
	}
}

// buildHeaders constructs WebSocket headers for sandbox registration.
func buildHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Sandbox-Name", *name)
	h.Set("X-Sandbox-Arch", runtime.GOARCH)
	h.Set("X-Sandbox-OS", runtime.GOOS)
	h.Set("X-Sandbox-OS-Version", getOSVersion())
	if *token != "" {
		h.Set("Authorization", "Bearer "+*token)
	}
	return h
}

// toWS converts http(s):// to ws(s)://
func toWS(u string) string {
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + u[len("https://"):]
	case strings.HasPrefix(u, "http://"):
		return "ws://" + u[len("http://"):]
	}
	return u
}

// getOSVersion reads PRETTY_NAME from /etc/os-release (Linux) or falls back
// to runtime.GOOS on other platforms (macOS, Windows).
func getOSVersion() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			val := strings.TrimPrefix(line, "PRETTY_NAME=")
			val = strings.Trim(val, `"`)
			return val
		}
	}
	return runtime.GOOS
}
