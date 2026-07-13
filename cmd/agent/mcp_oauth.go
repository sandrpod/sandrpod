// Copyright 2024 SandrPod
// mcp_oauth.go — agent-side wiring for the bridge's native OAuth flow
// (docs/MCP_AUTH.md). The agent runs in the employee's user session, so it can
// hand the authorization URL straight to the system browser: adding an
// OAuth-protected MCP server (Notion/GitHub/Linear-style) to mcp.json pops the
// consent page; the loopback callback stores the token and the server comes up.

package main

import (
	"log"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/sandrpod/sandrpod/pkg/homedir"
	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
)

// buildMCPOAuthOptions assembles the bridge OAuth options from flags/env.
// Returns nil when disabled (-mcp-oauth=false), which makes auth=oauth
// entries fail with a clear error instead of silently hanging.
func buildMCPOAuthOptions() *mcpbridge.OAuthOptions {
	if !*mcpOAuth {
		return nil
	}
	dir := *mcpOAuthTokenDir
	if dir == "" {
		dir = filepath.Join(homedir.DataDir(), "oauth")
	}
	return &mcpbridge.OAuthOptions{
		TokenDir:     dir,
		CallbackAddr: *mcpOAuthCallback,
		OnAuthorizationRequired: func(server, authURL string) {
			log.Printf("MCP bridge: server %q requires authorization — opening browser: %s", server, authURL)
			if err := openBrowser(authURL); err != nil {
				log.Printf("MCP bridge: could not open browser (%v); open this URL manually: %s", err, authURL)
			}
		},
	}
}

// openBrowser opens url in the user's default browser. The agent runs in the
// user session (not a system service), so this works on all three desktops.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
