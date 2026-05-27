package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sandrpod/sandrpod/pkg/audit"
	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
	"github.com/sandrpod/sandrpod/pkg/permission"
)

// mcpAuditAdapter forwards mcpbridge audit events into pkg/audit. We pack
// MCP-specific fields into the existing Event schema (no schema change) by
// using Source "mcp.spawn" / "mcp.call" / "mcp.restart" and stashing
// server/tool/status into Path/Caller/Reason.
//
// Why pack rather than extend the schema?
//   The audit upload wire format is versioned and consumed by the central
//   platform. Adding fields requires a coordinated rollout. Packing keeps
//   the bridge shippable today; we can split fields out later when v2 of
//   the wire format ships.
type mcpAuditAdapter struct {
	rec *audit.Recorder
}

func (a *mcpAuditAdapter) Record(ev mcpbridge.AuditEvent) {
	if a == nil || a.rec == nil {
		return
	}

	reason := redactReason(ev)
	caller := "mcp.bridge"
	if ev.Tool != "" {
		caller = "mcp.bridge:" + ev.Tool
	}

	_ = a.rec.Record(audit.Event{
		Source:   audit.Source(ev.Source),
		Decision: string(ev.Decision),
		Path:     "mcp:" + ev.Server, // grouping key — never a real path
		Caller:   caller,
		Reason:   reason,
	})
}

// redactReason assembles the free-text reason line. We never log argsSummary
// or result content — even truncated, args may contain customer data.
func redactReason(ev mcpbridge.AuditEvent) string {
	var b strings.Builder
	if ev.ResultStatus != "" {
		b.WriteString("status=")
		b.WriteString(ev.ResultStatus)
	}
	if ev.DurationMs > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "duration_ms=%d", ev.DurationMs)
	}
	if ev.Reason != "" {
		if b.Len() > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(ev.Reason)
	}
	return b.String()
}

// mcpPermissionAdapter wraps the same notifier the path-permission gate
// uses (typically the tray IPC client) and persists allow_permanent
// decisions in a dedicated mcp_grants.json next to permissions.json.
//
// We keep MCP grants in a SEPARATE file so the existing permissions.json
// schema (consumed by tray UI and tested by other code) stays stable. The
// new file is the bridge's private store.
type mcpPermissionAdapter struct {
	notifier  permission.Notifier
	storePath string

	mu     sync.Mutex
	grants mcpGrants
}

type mcpGrants struct {
	Version   int             `json:"version"`
	Servers   map[string]bool `json:"servers"`              // server name -> persistent allow for mcp.spawn
	Tools     map[string]bool `json:"tools,omitempty"`      // "server:tool" -> persistent allow for mcp.call
	UpdatedAt time.Time       `json:"updated_at"`
}

func newMCPPermissionAdapter(notifier permission.Notifier, storePath string) (*mcpPermissionAdapter, error) {
	a := &mcpPermissionAdapter{
		notifier:  notifier,
		storePath: storePath,
		grants: mcpGrants{
			Version: 1,
			Servers: map[string]bool{},
			Tools:   map[string]bool{},
		},
	}
	if err := a.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return a, nil
}

func (a *mcpPermissionAdapter) load() error {
	data, err := os.ReadFile(a.storePath)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var g mcpGrants
	if err := json.Unmarshal(data, &g); err != nil {
		return fmt.Errorf("parse mcp_grants.json: %w", err)
	}
	if g.Servers == nil {
		g.Servers = map[string]bool{}
	}
	if g.Tools == nil {
		g.Tools = map[string]bool{}
	}
	a.grants = g
	return nil
}

func (a *mcpPermissionAdapter) flushLocked() error {
	a.grants.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(a.grants, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.storePath), 0o700); err != nil {
		return err
	}
	tmp := a.storePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.storePath)
}

// Check is the PermissionGate impl. Returns Allow immediately for already-
// granted entries; otherwise prompts via the notifier and (on
// PromptAllowPermanent) persists. Failure modes are fail-close (deny).
//
// Sensitive tools (delete_*, send_*, …) bypass the cache and prompt every
// time, even if the user previously chose "allow permanent". This is a
// safety floor: a permanent grant for a benign tool like list_issues
// should never accidentally cover delete_repo just because they share a
// server. See sensitiveToolPatterns below for the exact list.
func (a *mcpPermissionAdapter) Check(ctx context.Context, evt mcpbridge.PermissionEvent) (mcpbridge.Decision, error) {
	sensitive := evt.Source == "mcp.call" && isSensitiveTool(evt.Tool)

	a.mu.Lock()
	if evt.Source == "mcp.call" && !sensitive {
		key := evt.Server + ":" + evt.Tool
		if a.grants.Tools[key] {
			a.mu.Unlock()
			return mcpbridge.DecisionAllow, nil
		}
	} else if evt.Source != "mcp.call" && a.grants.Servers[evt.Server] {
		// mcp.spawn / mcp.restart inherit the server-level grant.
		a.mu.Unlock()
		return mcpbridge.DecisionAllow, nil
	}
	a.mu.Unlock()

	// Build a Request the notifier understands. The path field is a
	// synthetic identifier so the tray UI's "what is being asked about"
	// label reads naturally.
	req := permission.Request{
		Path:   "mcp:" + evt.Server,
		Mode:   permission.ModeReadWrite,
		Reason: describeEvent(evt),
		Caller: evt.Source,
	}

	resp, err := a.notifier.Ask(ctx, req)
	if err != nil {
		return mcpbridge.DecisionDeny, err
	}

	switch resp {
	case permission.PromptAllowOnce, permission.PromptAllowSession:
		return mcpbridge.DecisionAllow, nil
	case permission.PromptAllowPermanent:
		// Sensitive tools never get a permanent grant — even if the
		// user picks "allow permanent" in the dialog. The reasoning:
		// a destructive tool's risk profile per-call is high enough
		// that we'd rather make the user click through every time than
		// have a single misclick turn into a standing authorization.
		if sensitive {
			return mcpbridge.DecisionAllow, nil
		}
		a.mu.Lock()
		if evt.Source == "mcp.call" {
			a.grants.Tools[evt.Server+":"+evt.Tool] = true
		} else {
			a.grants.Servers[evt.Server] = true
		}
		_ = a.flushLocked()
		a.mu.Unlock()
		return mcpbridge.DecisionAllow, nil
	case permission.PromptDeny:
		return mcpbridge.DecisionDeny, nil
	case permission.PromptTimeout:
		return mcpbridge.DecisionDeny, fmt.Errorf("permission prompt timed out")
	}
	return mcpbridge.DecisionDeny, fmt.Errorf("unexpected prompt response %q", resp)
}

func describeEvent(evt mcpbridge.PermissionEvent) string {
	switch evt.Source {
	case "mcp.spawn":
		envHint := ""
		if len(evt.EnvKeys) > 0 {
			envHint = " (env: " + strings.Join(evt.EnvKeys, ", ") + ")"
		}
		return fmt.Sprintf("Run MCP server %q with command %s %s%s",
			evt.Server, evt.Command, strings.Join(evt.Args, " "), envHint)
	case "mcp.call":
		return fmt.Sprintf("Invoke tool %q on MCP server %q", evt.Tool, evt.Server)
	case "mcp.restart":
		return fmt.Sprintf("Restart MCP server %q", evt.Server)
	}
	return evt.Source + " " + evt.Server
}

// sensitiveToolPatterns is the built-in list of substrings that mark a
// tool as destructive / irreversible / outbound-side-effect. Matching is
// case-insensitive on the un-prefixed tool name (e.g. "delete_repo",
// not "github__delete_repo").
//
// The list is intentionally conservative — false positives mean an extra
// prompt, false negatives mean a permanent grant on something the user
// would have wanted to confirm. We bias toward the former.
//
// Extend via SANDRPOD_MCP_SENSITIVE_PATTERNS env (comma-separated).
// Replace (not extend) via SANDRPOD_MCP_SENSITIVE_PATTERNS_OVERRIDE.
var sensitiveToolPatterns = []string{
	"delete",
	"remove",
	"drop",
	"truncate",
	"purge",
	"destroy",
	"wipe",
	"send",      // send_email, send_message, send_dm, ...
	"publish",   // publish_post, publish_repo, ...
	"post",      // post_tweet, post_comment, ... (some false positives — OK)
	"transfer",
	"pay",
	"charge",
	"merge",     // merge_pr — irreversible from user POV
	"revoke",
	"reset",     // reset_password
	"unsubscribe",
}

func isSensitiveTool(toolName string) bool {
	if toolName == "" {
		return false
	}
	lower := strings.ToLower(toolName)
	for _, p := range sensitivePatternsRuntime() {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// sensitivePatternsRuntime resolves env overrides on each call. We re-read
// env every time rather than caching so operators can change the env
// (e.g. via tray restart of the agent) without rebuilding. The cost is
// trivial — Check is not on the hot path of an MCP tool, the underlying
// child call dominates by orders of magnitude.
func sensitivePatternsRuntime() []string {
	if override := os.Getenv("SANDRPOD_MCP_SENSITIVE_PATTERNS_OVERRIDE"); override != "" {
		return splitCSVLower(override)
	}
	base := sensitiveToolPatterns
	if extra := os.Getenv("SANDRPOD_MCP_SENSITIVE_PATTERNS"); extra != "" {
		base = append(append([]string{}, base...), splitCSVLower(extra)...)
	}
	return base
}

func splitCSVLower(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.ToLower(strings.TrimSpace(p)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// defaultMCPGrantsPath returns the conventional store location.
func defaultMCPGrantsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "mcp_grants.json"
	}
	return filepath.Join(home, ".sandrpod", "mcp_grants.json")
}
