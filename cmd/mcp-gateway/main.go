// Command mcp-gateway is a drop-in for E2B's in-sandbox `mcp-gateway` binary,
// so the unmodified E2B SDK's Sandbox.create({ mcp: {...} }) works against
// sandrpod: the SDK, after creating a sandbox, runs
//
//	mcp-gateway --config '<json>'      (as root, env GATEWAY_ACCESS_TOKEN=<token>)
//
// inside the sandbox, then connects an MCP client to
// https://50005-<id>.<domain>/mcp with `Authorization: Bearer <token>`, and
// reads the token back via getMcpToken() → files.read('/etc/mcp-gateway/.token').
//
// This shim reproduces that contract using sandrpod's own MCP aggregator
// (pkg/mcpbridge): it translates E2B's `mcp` config into a mcpbridge config,
// serves the aggregated servers over MCP Streamable-HTTP on :50005 at /mcp
// (bearer-guarded by GATEWAY_ACCESS_TOKEN), and writes that token to
// /etc/mcp-gateway/.token. No E2B-specific server-side code is needed — the URL
// is client-formatted and the token/command flow rides the envd files/commands
// surface sandrpod already implements.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
)

const (
	defaultPort = 50005
	// defaultStateDir holds this gateway's token + config. It is deliberately
	// NOT mcpbridge.DefaultConfigPath(): inside the toolbox image that path is
	// /workspace/.sandrpod/mcp.json, which the toolbox's own built-in bridge
	// (:8080/mcp) reads. A separate dir keeps the E2B gateway (:50005) and
	// sandrpod's native bridge fully independent. The default MUST stay
	// /etc/mcp-gateway because E2B's getMcpToken() reads /etc/mcp-gateway/.token;
	// the -state-dir flag / MCP_GATEWAY_STATE_DIR only relocates it for
	// non-root local runs and custom deployments.
	defaultStateDir = "/etc/mcp-gateway"
	tokenFile       = ".token"
	configFile      = "config.json"
)

func main() {
	configJSON := flag.String("config", "", "MCP config: E2B `mcp` map shape or sandrpod {mcpServers:{…}} shape")
	port := flag.Int("port", defaultPort, "MCP Streamable-HTTP listen port")
	stateDir := flag.String("state-dir", envOr("MCP_GATEWAY_STATE_DIR", defaultStateDir),
		"directory for the gateway's config + token (default /etc/mcp-gateway; E2B reads .token here)")
	flag.Parse()

	token := os.Getenv("GATEWAY_ACCESS_TOKEN")
	cfgPath := filepath.Join(*stateDir, configFile)
	tokenPath := filepath.Join(*stateDir, tokenFile)

	cfg, err := translateConfig(*configJSON)
	if err != nil {
		log.Fatalf("mcp-gateway: invalid --config: %v", err)
	}

	// The manager reads a config file (and hot-reloads it), so persist the
	// translated config to this gateway's dedicated path.
	if err := writeConfig(cfgPath, cfg); err != nil {
		log.Fatalf("mcp-gateway: write config %s: %v", cfgPath, err)
	}

	// Persist the token where the E2B SDK's getMcpToken() reads it.
	if token != "" {
		if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
			log.Printf("mcp-gateway: warning: cannot create %s: %v", filepath.Dir(tokenPath), err)
		} else if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
			log.Printf("mcp-gateway: warning: cannot write %s: %v", tokenPath, err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mgr := mcpbridge.NewManager(mcpbridge.ManagerOptions{
		ConfigPath: cfgPath,
		HotReload:  true,
		Logger:     log.Default(),
	})
	if err := mgr.Start(ctx); err != nil {
		log.Fatalf("mcp-gateway: start bridge: %v", err)
	}

	var handler http.Handler = mcpbridge.NewHTTPHandler(mgr)
	if token != "" {
		handler = bearerMiddleware(token, handler)
	} else {
		log.Printf("mcp-gateway: WARNING — no GATEWAY_ACCESS_TOKEN; /mcp is unauthenticated")
	}

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Printf("mcp-gateway: MCP Streamable-HTTP on :%d/mcp (auth=%v, servers=%d)", *port, token != "", len(mgr.Snapshot()))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("mcp-gateway: serve: %v", err)
	}
}

// bearerMiddleware enforces a constant-time Bearer check on every request, matching
// how E2B's gateway guards /mcp with GATEWAY_ACCESS_TOKEN.
func bearerMiddleware(secret string, next http.Handler) http.Handler {
	want := "Bearer " + secret
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// envOr returns the environment variable value or a fallback when unset/empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func writeConfig(path string, cfg *mcpbridge.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
