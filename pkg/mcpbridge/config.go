package mcpbridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Config is the on-disk mcp.json layout. It is intentionally compatible with
// Claude Desktop / Cursor / Cline: any field they understand is preserved,
// and sandrpod-specific knobs live inside the "sandrpod" sub-object so other
// tools simply ignore them.
type Config struct {
	McpServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig describes a single MCP server entry. A server is either:
//   - stdio: set Command (+ Args/Env) — spawned as a subprocess, or
//   - HTTP:  set URL (+ Type/Headers) — connected over Streamable-HTTP or SSE.
//
// Exactly one of Command / URL must be set. The HTTP fields (url/type/headers)
// mirror Claude Code's remote-server shape for config compatibility.
//
// OAuth-protected HTTP servers (MCP authorization spec: OAuth 2.1 + PKCE +
// dynamic client registration — Notion/GitHub/Linear-style endpoints) opt in
// with `"auth": "oauth"`. The bridge then runs the browser-consent flow once
// (child parks in waiting_auth with an authorization URL), persists the token,
// and refreshes it unattended. See docs/MCP_AUTH.md. The stdio `mcp-remote`
// shim remains a valid alternative.
type ServerConfig struct {
	// stdio transport
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// HTTP transport
	URL     string            `json:"url,omitempty"`     // remote MCP endpoint
	Type    string            `json:"type,omitempty"`    // "streamable-http" (default) | "http" | "sse"
	Headers map[string]string `json:"headers,omitempty"` // request headers; values support ${ENV} expansion

	// Auth selects the auth mechanism for an HTTP entry. "" (default) means
	// static Headers only; "oauth" opts into the browser OAuth flow.
	Auth string `json:"auth,omitempty"`
	// OAuth carries optional OAuth details; its presence also implies
	// Auth=oauth. Nil is the norm (dynamic client registration).
	OAuth *OAuthServerOpts `json:"oauth,omitempty"`

	// Sandrpod holds bridge-specific options. Optional; nil means defaults.
	Sandrpod *SandrpodOpts `json:"sandrpod,omitempty"`
}

// OAuthServerOpts pins OAuth client details for servers that don't support
// dynamic registration, and requests non-default scopes. Values support
// ${ENV} expansion like Headers.
type OAuthServerOpts struct {
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// IsHTTP reports whether this entry is an HTTP (url-based) server.
func (s ServerConfig) IsHTTP() bool { return strings.TrimSpace(s.URL) != "" }

// WantsOAuth reports whether this entry opted into the browser OAuth flow.
func (s ServerConfig) WantsOAuth() bool {
	return strings.EqualFold(strings.TrimSpace(s.Auth), "oauth") || s.OAuth != nil
}

// Target returns a human-readable target (command or url) for logs/manifest.
func (s ServerConfig) Target() string {
	if s.IsHTTP() {
		return s.URL
	}
	return s.Command
}

// Validate checks that exactly one transport is configured.
func (s ServerConfig) Validate() error {
	hasCmd := strings.TrimSpace(s.Command) != ""
	hasURL := s.IsHTTP()
	switch {
	case hasCmd && hasURL:
		return fmt.Errorf("both command and url set; specify exactly one")
	case !hasCmd && !hasURL:
		return fmt.Errorf("neither command nor url set")
	}
	if s.WantsOAuth() {
		if !hasURL {
			return fmt.Errorf("auth=oauth requires an http (url) server")
		}
		if t := strings.ToLower(strings.TrimSpace(s.Type)); t == "sse" {
			return fmt.Errorf("auth=oauth requires the streamable-http transport, not sse")
		}
	}
	return nil
}

// SandrpodOpts carries sandrpod-specific runtime options for a server.
type SandrpodOpts struct {
	Enabled           *bool    `json:"enabled,omitempty"`
	Alias             string   `json:"alias,omitempty"`
	RestartPolicy     string   `json:"restart_policy,omitempty"`
	MaxRestartPerMin  int      `json:"max_restart_per_min,omitempty"`
	StartupTimeoutSec int      `json:"startup_timeout_sec,omitempty"`
	ToolAllowlist     []string `json:"tool_allowlist,omitempty"`
	ToolDenylist      []string `json:"tool_denylist,omitempty"`
}

// IsEnabled reports whether the server should be spawned. Defaults to true
// when the sandrpod sub-object is absent or .enabled is unset.
func (s ServerConfig) IsEnabled() bool {
	if s.Sandrpod == nil || s.Sandrpod.Enabled == nil {
		return true
	}
	return *s.Sandrpod.Enabled
}

// AliasOr returns the configured alias, falling back to fallback (typically
// the mcp.json key).
func (s ServerConfig) AliasOr(fallback string) string {
	if s.Sandrpod != nil && s.Sandrpod.Alias != "" {
		return s.Sandrpod.Alias
	}
	return fallback
}

// LoadConfig reads and parses an mcp.json file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mcp config %s: %w", path, err)
	}
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse mcp config %s: %w", path, err)
	}
	if cfg.McpServers == nil {
		cfg.McpServers = map[string]ServerConfig{}
	}
	return cfg, nil
}

// SortedKeys returns the server keys in deterministic order. Stable naming
// matters because the aggregator uses the order to break tool-name ties.
func (c *Config) SortedKeys() []string {
	keys := make([]string, 0, len(c.McpServers))
	for k := range c.McpServers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DefaultConfigPath returns the platform-conventional mcp.json location.
//
//	macOS / Linux: $XDG_CONFIG_HOME/sandrpod/mcp.json, falling back to
//	               ~/.sandrpod/mcp.json when XDG isn't set.
//	Windows:       %APPDATA%\sandrpod\mcp.json
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "sandrpod", "mcp.json")
		}
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "sandrpod", "mcp.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "mcp.json"
	}
	return filepath.Join(home, ".sandrpod", "mcp.json")
}
