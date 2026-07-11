package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
//
//	The audit upload wire format is versioned and consumed by the central
//	platform. Adding fields requires a coordinated rollout. Packing keeps
//	the bridge shippable today; we can split fields out later when v2 of
//	the wire format ships.
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
	scope     grantScope

	mu     sync.Mutex
	grants mcpGrants
	// seen is the (mtime, size) of the store as of the last read — the
	// zero value means "file absent". loadIfChanged compares against it
	// so hand-edits are picked up without re-parsing on every call.
	seen fileStamp
	// Session grants ("allow for this session" in the dialog) live here —
	// in-memory only, cleared when the agent restarts. Mirrors the
	// persistent store's two-map split so a server literally named
	// "gh:list_issues" can never collide with a tool key.
	sessionServers map[string]bool
	sessionTools   map[string]bool // "server:tool"
}

// fileStamp identifies a store-file revision. os.FileInfo mtimes carry no
// monotonic clock, so == comparison is sound.
type fileStamp struct {
	mod  time.Time
	size int64
}

// grantScope is the granularity of grants issued from the consent dialog.
// It shapes what a click WRITES, never what a lookup honors: existing
// per-tool entries, wildcards and session grants all keep working in
// either mode, and sensitive tools prompt every time in both.
type grantScope string

const (
	// grantScopeServer: one allow covers every non-sensitive tool on that
	// server (persisted as the "server:*" wildcard). The default — first
	// use of a server prompts once, then it goes quiet.
	grantScopeServer grantScope = "server"
	// grantScopeTool: today's narrow behavior — every tool prompts once.
	// For deployments where each tool is a separately-audited capability.
	grantScopeTool grantScope = "tool"
)

// parseGrantScope maps a flag/env value to a scope. Unknown values fall
// back to the NARROW scope (more prompts, never wider access) and log.
func parseGrantScope(s string) grantScope {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(grantScopeServer):
		return grantScopeServer
	case string(grantScopeTool):
		return grantScopeTool
	default:
		log.Printf("MCP bridge: unknown -mcp-grant-scope %q — falling back to per-tool grants", s)
		return grantScopeTool
	}
}

type mcpGrants struct {
	Version int             `json:"version"`
	Servers map[string]bool `json:"servers"` // server name -> persistent allow for mcp.spawn
	// Tools maps "server:tool" -> persistent allow for mcp.call. The
	// special key "server:*" pre-approves every NON-sensitive tool on that
	// server (sensitive ones still prompt — see isSensitiveTool). Wildcards
	// are operator-authored (edit the file); the dialog never writes one.
	Tools     map[string]bool `json:"tools,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func newMCPPermissionAdapter(notifier permission.Notifier, storePath string, scope grantScope) *mcpPermissionAdapter {
	a := &mcpPermissionAdapter{
		notifier:  notifier,
		storePath: storePath,
		scope:     scope,
		grants: mcpGrants{
			Version: 1,
			Servers: map[string]bool{},
			Tools:   map[string]bool{},
		},
		sessionServers: map[string]bool{},
		sessionTools:   map[string]bool{},
	}
	a.loadIfChanged()
	return a
}

// loadIfChanged synchronises in-memory grants with the on-disk store, so
// hand-edits take effect without an agent restart — in BOTH directions:
// a new grant is honored on the next check, and deleting the file revokes
// every persistent grant. Construction and every Check go through here.
//
// Cost control: the file is re-read only when its (mtime, size) changed
// since the last observation; the steady-state cost is one os.Stat, ~µs
// against the ~ms child tool call each check guards.
//
// Failure modes degrade safely: a corrupt file keeps the last good state
// (logged once per file revision, not per call — a broken permission
// store must never widen access, and the old constructor error path did
// exactly that by collapsing to the bridge's allow-all default). Session
// grants are in-memory only and untouched throughout.
func (a *mcpPermissionAdapter) loadIfChanged() {
	a.mu.Lock()
	defer a.mu.Unlock()

	fi, err := os.Stat(a.storePath)
	if os.IsNotExist(err) {
		if a.seen != (fileStamp{}) {
			a.grants = mcpGrants{Version: 1, Servers: map[string]bool{}, Tools: map[string]bool{}}
			a.seen = fileStamp{}
			log.Printf("MCP bridge: %s removed — all persistent MCP grants revoked", a.storePath)
		}
		return
	}
	if err != nil {
		log.Printf("MCP bridge: stat %s: %v — keeping previously loaded grants", a.storePath, err)
		return
	}
	stamp := fileStamp{mod: fi.ModTime(), size: fi.Size()}
	if stamp == a.seen {
		return
	}
	// Record the revision even when it fails to parse below, so a broken
	// file logs once instead of on every subsequent check.
	a.seen = stamp

	data, err := os.ReadFile(a.storePath)
	if err != nil {
		log.Printf("MCP bridge: read %s: %v — keeping previously loaded grants", a.storePath, err)
		return
	}
	var g mcpGrants
	if err := json.Unmarshal(data, &g); err != nil {
		log.Printf("MCP bridge: parse %s: %v — keeping previously loaded grants", a.storePath, err)
		return
	}
	if g.Servers == nil {
		g.Servers = map[string]bool{}
	}
	if g.Tools == nil {
		g.Tools = map[string]bool{}
	}
	a.grants = g
}

// grantedLocked reports whether evt is covered by a persistent or session
// grant. Caller must hold a.mu. Both maps honor the exact tool key and
// the "server:*" wildcard regardless of the configured scope — scope only
// decides which of the two a dialog click writes.
func (a *mcpPermissionAdapter) grantedLocked(evt mcpbridge.PermissionEvent) bool {
	if evt.Source == "mcp.call" {
		key := evt.Server + ":" + evt.Tool
		wild := evt.Server + ":*"
		return a.grants.Tools[key] || a.grants.Tools[wild] ||
			a.sessionTools[key] || a.sessionTools[wild]
	}
	// mcp.spawn / mcp.restart share the server-level grant.
	return a.grants.Servers[evt.Server] || a.sessionServers[evt.Server]
}

// callGrantKey is the key a dialog allow is recorded under for mcp.call
// events: the whole-server wildcard in server scope, the exact tool in
// tool scope.
func (a *mcpPermissionAdapter) callGrantKey(evt mcpbridge.PermissionEvent) string {
	if a.scope == grantScopeServer {
		return evt.Server + ":*"
	}
	return evt.Server + ":" + evt.Tool
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

// Check is the PermissionGate impl. Resolution order for non-sensitive
// events: persistent grant (exact key, then the "server:*" wildcard for
// calls) → session grant (in-memory, gone on agent restart) → prompt.
// PromptAllowPermanent persists; PromptAllowSession caches in memory
// only. Failure modes are fail-close (deny).
//
// Sensitive tools (delete_*, send_*, …) bypass every cache — exact,
// wildcard and session — and prompt on each call, even if the user
// previously chose "allow permanent". This is a safety floor: a standing
// grant for a benign tool like list_issues (or a whole-server wildcard)
// should never accidentally cover delete_repo just because they share a
// server. See sensitiveToolPatterns below for the exact list.
func (a *mcpPermissionAdapter) Check(ctx context.Context, evt mcpbridge.PermissionEvent) (mcpbridge.Decision, error) {
	sensitive := evt.Source == "mcp.call" && isSensitiveTool(evt.Tool)

	if !sensitive {
		a.loadIfChanged()
		a.mu.Lock()
		granted := a.grantedLocked(evt)
		a.mu.Unlock()
		if granted {
			return mcpbridge.DecisionAllow, nil
		}
	}

	// Build a Request the notifier understands. The path field is a
	// synthetic identifier so the tray UI's "what is being asked about"
	// label reads naturally.
	reason := describeEvent(evt)
	if a.scope == grantScopeServer && evt.Source == "mcp.call" && !sensitive {
		// The click grants more than this one tool — say so in the dialog.
		reason += " — allowing covers ALL non-sensitive tools on this server"
	}
	req := permission.Request{
		Path:   "mcp:" + evt.Server,
		Mode:   permission.ModeReadWrite,
		Reason: reason,
		Caller: evt.Source,
	}

	resp, err := a.notifier.Ask(ctx, req)
	if err != nil {
		return mcpbridge.DecisionDeny, err
	}

	switch resp {
	case permission.PromptAllowOnce:
		return mcpbridge.DecisionAllow, nil
	case permission.PromptAllowSession:
		// Remember until the agent restarts. Sensitive tools are never
		// cached — same reasoning as the permanent case below.
		if !sensitive {
			a.mu.Lock()
			if evt.Source == "mcp.call" {
				a.sessionTools[a.callGrantKey(evt)] = true
			} else {
				a.sessionServers[evt.Server] = true
			}
			a.mu.Unlock()
		}
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
		// Merge the on-disk state first: flushLocked writes the whole
		// struct, so without this a hand-edit made while the agent runs
		// (e.g. an operator-added "server:*" wildcard) would be clobbered
		// by the next dialog click.
		a.loadIfChanged()
		a.mu.Lock()
		if evt.Source == "mcp.call" {
			a.grants.Tools[a.callGrantKey(evt)] = true
		} else {
			a.grants.Servers[evt.Server] = true
		}
		if err := a.flushLocked(); err != nil {
			log.Printf("MCP bridge: persist grant failed (%v) — grant holds for this run only", err)
		}
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
	// Destructive / irreversible
	"delete",
	"remove",
	"drop",
	"truncate",
	"purge",
	"destroy",
	"wipe",
	"clear", // clear_history, clear_data
	"kill",  // kill_process, kill_job
	"reset", // reset_password
	"cancel",
	"archive",
	// Outbound side effects
	"send",    // send_email, send_message, send_dm, ...
	"publish", // publish_post, publish_repo, ...
	"post",    // post_tweet, post_comment, ... (some false positives — OK)
	"invite",
	"share", // share grants access to others
	// Financial
	"transfer",
	"pay",
	"charge",
	"withdraw",
	"downgrade",
	// Access / moderation / state
	"merge", // merge_pr — irreversible from user POV
	"revoke",
	"grant",   // grant_access, grant_role
	"approve", // approve_pr, approve_payment
	"block",
	"ban",
	"disable",
	"deactivate",
	"suspend",
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
