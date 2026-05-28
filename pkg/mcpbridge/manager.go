package mcpbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mark3labs/mcp-go/mcp"
)

// RestartPolicy values mirror systemd / Docker conventions.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

const (
	defaultRestartPolicy    = RestartAlways
	defaultMaxRestartPerMin = 3
	defaultPingInterval     = 20 * time.Second
	defaultPingTimeout      = 5 * time.Second

	// Restart backoff parameters. A child that died once gets back up
	// quickly (1s); each consecutive failure doubles the wait. The cap
	// is below the per-minute restart limit so the rate-limit gate is
	// what ultimately stops a hopelessly-broken child, not the backoff.
	restartBackoffBase = 1 * time.Second
	restartBackoffMax  = 30 * time.Second
)

// ManagerOptions configures a ChildManager.
type ManagerOptions struct {
	ConfigPath string
	Permission PermissionGate
	Audit      AuditSink
	Logger     *log.Logger

	// HotReload enables fsnotify on ConfigPath. The manager will diff the new
	// config against the running set and restart only changed entries.
	HotReload bool

	// SupervisorInterval is the per-child ping interval used to detect dead
	// stdio children. Zero uses defaultPingInterval.
	SupervisorInterval time.Duration
}

// ChildManager owns the set of stdio children.
type ChildManager struct {
	opts ManagerOptions

	mu       sync.RWMutex
	children map[string]*Child
	fqIndex  map[string]fqEntry

	onChange []func()

	// supervisor goroutine bookkeeping
	supervisorCtx    context.Context
	supervisorCancel context.CancelFunc
	watcher          *fsnotify.Watcher
}

type fqEntry struct {
	childName    string
	originalName string
}

// NewManager constructs a manager but does not start it.
func NewManager(opts ManagerOptions) *ChildManager {
	if opts.Permission == nil {
		opts.Permission = allowAllGate{}
	}
	if opts.Audit == nil {
		opts.Audit = nopAuditSink{}
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.SupervisorInterval <= 0 {
		opts.SupervisorInterval = defaultPingInterval
	}
	return &ChildManager{
		opts:     opts,
		children: map[string]*Child{},
		fqIndex:  map[string]fqEntry{},
	}
}

// Start loads the config, spawns every enabled child, and (optionally)
// arms the supervisor + fsnotify watcher.
//
// Two failure modes are tolerated by design:
//
//  1. A single child failing to spawn — the others continue, the failed
//     one is marked Failed and shows up in /mcp/manifest with last_error.
//  2. The config file being absent at start time — the manager comes up
//     with zero children, the watcher is still armed on the parent dir,
//     and a later create/write triggers a normal reload. This makes
//     "install sandrpod-agent before creating mcp.json" work the way
//     users actually do it, rather than failing the bridge permanently.
//
// Parse errors (file present but malformed JSON) still surface as a
// hard error — silently swallowing bad JSON would mask configuration
// mistakes that the user wants to fix.
func (m *ChildManager) Start(ctx context.Context) error {
	cfg, err := LoadConfig(m.opts.ConfigPath)
	switch {
	case err == nil:
		m.mu.Lock()
		for _, name := range cfg.SortedKeys() {
			sc := cfg.McpServers[name]
			m.spawnLocked(ctx, name, sc)
		}
		m.rebuildIndexLocked()
		m.mu.Unlock()
	case errors.Is(err, os.ErrNotExist):
		m.opts.Logger.Printf("mcpbridge: config %s not present yet — starting with no servers; will pick up on create",
			m.opts.ConfigPath)
	default:
		return err
	}

	// Long-lived supervisor uses a derived context independent of the
	// passed-in ctx so the caller can let ctx outlive a single request.
	m.supervisorCtx, m.supervisorCancel = context.WithCancel(context.Background())
	go m.supervisorLoop(m.supervisorCtx)

	if m.opts.HotReload {
		if err := m.startWatcherLocked(); err != nil {
			m.opts.Logger.Printf("mcpbridge: fsnotify watcher failed: %v (hot-reload disabled)", err)
		}
	}
	return nil
}

// Stop is the abrupt-termination path: cancels the supervisor, closes
// the watcher, kills every child. In-flight tools/call invocations are
// abandoned. Prefer Shutdown for graceful termination.
func (m *ChildManager) Stop(ctx context.Context) error {
	if m.supervisorCancel != nil {
		m.supervisorCancel()
	}
	if m.watcher != nil {
		_ = m.watcher.Close()
		m.watcher = nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for name, c := range m.children {
		if err := c.Stop(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stop %s: %w", name, err)
		}
	}
	m.children = map[string]*Child{}
	m.fqIndex = map[string]fqEntry{}
	return firstErr
}

// Shutdown drains in-flight tools/call invocations up to drainTimeout,
// then tears everything down. The supervisor and watcher are stopped
// first so no new restart attempts or reloads kick in during drain.
//
// Returns nil on clean drain; an error describing which children didn't
// finish in time otherwise (but Stop() is still run regardless so the
// caller can ignore the error and exit).
func (m *ChildManager) Shutdown(ctx context.Context, drainTimeout time.Duration) error {
	// Halt anything that might spawn or restart children during drain.
	if m.supervisorCancel != nil {
		m.supervisorCancel()
	}
	if m.watcher != nil {
		_ = m.watcher.Close()
		m.watcher = nil
	}

	// Snapshot children under read lock so we can drain without
	// blocking new ones (there shouldn't be any after the supervisor
	// is stopped, but mid-flight calls keep the WaitGroup busy).
	m.mu.RLock()
	pending := make([]*Child, 0, len(m.children))
	for _, c := range m.children {
		pending = append(pending, c)
	}
	m.mu.RUnlock()

	drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	var notDrained []string
	for _, c := range pending {
		if !c.WaitDrain(drainCtx) {
			notDrained = append(notDrained, c.Name)
		}
	}

	// Now kill the children — clean or not.
	m.mu.Lock()
	for _, c := range m.children {
		_ = c.Stop(ctx)
	}
	m.children = map[string]*Child{}
	m.fqIndex = map[string]fqEntry{}
	m.mu.Unlock()

	if len(notDrained) > 0 {
		return fmt.Errorf("drain timeout exceeded for: %v", notDrained)
	}
	return nil
}

// Reload re-reads the config file and applies the diff: new entries are
// spawned, removed entries are stopped, modified entries (different
// command/args/env/sandrpod opts) are restarted. Unchanged entries keep
// their existing subprocess.
//
// File-absent is handled like an empty config (all children torn down),
// so `rm ~/.sandrpod/mcp.json` is a valid "shut down all my MCP servers"
// gesture and a later recreate brings them back.
func (m *ChildManager) Reload(ctx context.Context) error {
	cfg, err := LoadConfig(m.opts.ConfigPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		cfg = &Config{McpServers: map[string]ServerConfig{}}
	}
	wantKeys := map[string]struct{}{}
	for _, k := range cfg.SortedKeys() {
		wantKeys[k] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop removed.
	for name, c := range m.children {
		if _, keep := wantKeys[name]; !keep {
			m.opts.Logger.Printf("mcpbridge: reload — removing %q", name)
			_ = c.Stop(ctx)
			delete(m.children, name)
		}
	}
	// Add or restart-on-change.
	for _, name := range cfg.SortedKeys() {
		sc := cfg.McpServers[name]
		if existing, ok := m.children[name]; ok {
			if existing.configHash == hashServerConfig(sc) {
				continue // unchanged
			}
			m.opts.Logger.Printf("mcpbridge: reload — restarting %q (config changed)", name)
			_ = existing.Stop(ctx)
			delete(m.children, name)
		}
		m.spawnLocked(ctx, name, sc)
	}
	m.rebuildIndexLocked()
	return nil
}

// RestartServer forcibly restarts a single named child (e.g. tray UI button).
func (m *ChildManager) RestartServer(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}
	cfg := c.Cfg
	_ = c.Stop(ctx)
	delete(m.children, name)
	m.spawnLocked(ctx, name, cfg)
	m.rebuildIndexLocked()
	return nil
}

// DisableServer stops a server in-memory without writing back to mcp.json.
// Survives until the next Reload.
func (m *ChildManager) DisableServer(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}
	_ = c.Stop(ctx)
	delete(m.children, name)
	m.rebuildIndexLocked()
	return nil
}

// spawnLocked launches a single child and stores it in m.children. Caller
// must hold m.mu. Permission denials and spawn failures are recorded but
// never panic.
func (m *ChildManager) spawnLocked(ctx context.Context, name string, sc ServerConfig) {
	if !sc.IsEnabled() {
		m.opts.Logger.Printf("mcpbridge: server %q disabled, skipping", name)
		return
	}

	envKeys := make([]string, 0, len(sc.Env))
	for k := range sc.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	dec, gateErr := m.opts.Permission.Check(ctx, PermissionEvent{
		Source:  "mcp.spawn",
		Server:  name,
		Command: sc.Command,
		Args:    sc.Args,
		EnvKeys: envKeys,
	})
	if gateErr != nil || dec != DecisionAllow {
		reason := "permission denied"
		if gateErr != nil {
			reason = gateErr.Error()
		}
		m.opts.Logger.Printf("mcpbridge: skipping %q: %s", name, reason)
		m.opts.Audit.Record(AuditEvent{
			Source:   "mcp.spawn",
			Decision: DecisionDeny,
			Server:   name,
			Reason:   reason,
		})
		// Store a denied placeholder so manifest reflects what was skipped.
		c := newChild(name, sc)
		c.setFailedReason("permission denied: " + reason)
		m.children[name] = c
		return
	}

	child := newChild(name, sc)
	child.configHash = hashServerConfig(sc)
	if err := child.Start(ctx); err != nil {
		m.opts.Logger.Printf("mcpbridge: start %q failed: %v", name, err)
		m.opts.Audit.Record(AuditEvent{
			Source:   "mcp.spawn",
			Decision: DecisionAllow,
			Server:   name,
			Reason:   fmt.Sprintf("spawn failed: %v", err),
		})
	}
	m.children[name] = child
}

// AggregatedTools returns the union of all ready children's tools, with
// names rewritten to alias__tool form.
func (m *ChildManager) AggregatedTools() []mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]mcp.Tool, 0)
	names := make([]string, 0, len(m.children))
	for k := range m.children {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, name := range names {
		c := m.children[name]
		if c.State() != StateReady {
			continue
		}
		for _, t := range c.Tools() {
			fqName := fullyQualifiedName(c.Alias, t.Name)
			cloned := t
			cloned.Name = fqName
			if cloned.Description != "" {
				cloned.Description = "[" + c.Alias + "] " + cloned.Description
			} else {
				cloned.Description = "[" + c.Alias + "]"
			}
			out = append(out, cloned)
		}
	}
	return out
}

// Dispatch routes a tools/call by fully-qualified name to the owning child.
func (m *ChildManager) Dispatch(ctx context.Context, fqName string, args any) (*mcp.CallToolResult, error) {
	m.mu.RLock()
	entry, ok := m.fqIndex[fqName]
	c := m.children[entry.childName]
	m.mu.RUnlock()
	if !ok || c == nil {
		return nil, fmt.Errorf("unknown tool %q", fqName)
	}

	dec, gateErr := m.opts.Permission.Check(ctx, PermissionEvent{
		Source: "mcp.call",
		Server: c.Name,
		Tool:   entry.originalName,
	})
	if gateErr != nil || dec != DecisionAllow {
		reason := "permission denied"
		if gateErr != nil {
			reason = gateErr.Error()
		}
		m.opts.Audit.Record(AuditEvent{
			Source:   "mcp.call",
			Decision: DecisionDeny,
			Server:   c.Name,
			Tool:     entry.originalName,
			Reason:   reason,
		})
		return nil, fmt.Errorf("tool %q denied: %s", fqName, reason)
	}

	started := time.Now()
	res, err := c.CallTool(ctx, entry.originalName, args)
	status := "ok"
	if err != nil {
		status = "error"
	} else if res != nil && res.IsError {
		status = "tool_error"
	}
	m.opts.Audit.Record(AuditEvent{
		Source:       "mcp.call",
		Decision:     DecisionAllow,
		Server:       c.Name,
		Tool:         entry.originalName,
		ResultStatus: status,
		DurationMs:   time.Since(started).Milliseconds(),
	})
	return res, err
}

// Snapshot returns a read-only view of children for /mcp/manifest.
func (m *ChildManager) Snapshot() []ChildSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ChildSnapshot, 0, len(m.children))
	for _, c := range m.children {
		c.mu.RLock()
		out = append(out, ChildSnapshot{
			Name:      c.Name,
			Alias:     c.Alias,
			State:     string(c.state),
			Command:   c.Cfg.Command,
			ToolCount: len(c.tools),
			StartedAt: c.startedAt,
			Restarts:  c.restarts,
			LastError: c.lastError,
		})
		c.mu.RUnlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ChildSnapshot is a read-only view of a Child for /mcp/manifest.
type ChildSnapshot struct {
	Name      string    `json:"name"`
	Alias     string    `json:"alias"`
	State     string    `json:"state"`
	Command   string    `json:"command"`
	ToolCount int       `json:"tool_count"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Restarts  int       `json:"restart_count"`
	LastError string    `json:"last_error,omitempty"`
}

// OnChange registers a callback fired whenever the aggregated tool set may
// have changed.
func (m *ChildManager) OnChange(fn func()) {
	m.mu.Lock()
	m.onChange = append(m.onChange, fn)
	m.mu.Unlock()
}

func (m *ChildManager) notifyChange() {
	m.mu.RLock()
	cbs := append([]func(){}, m.onChange...)
	m.mu.RUnlock()
	for _, fn := range cbs {
		go fn()
	}
}

func (m *ChildManager) rebuildIndexLocked() {
	idx := map[string]fqEntry{}
	names := make([]string, 0, len(m.children))
	for k := range m.children {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		c := m.children[name]
		if c.State() != StateReady {
			continue
		}
		for _, t := range c.Tools() {
			fq := fullyQualifiedName(c.Alias, t.Name)
			// Conflict resolution: first writer (alphabetical) wins;
			// later collisions get a deterministic per-child suffix so
			// they stay stable across restarts.
			if _, exists := idx[fq]; exists {
				fq = fq + "__from_" + c.Name
			}
			idx[fq] = fqEntry{childName: c.Name, originalName: t.Name}
		}
	}
	m.fqIndex = idx
	// Notify outside the lock to avoid deadlock if a callback re-enters.
	go m.notifyChange()
}

// supervisorLoop pings ready children to detect crashes, and applies the
// restart policy. Runs until ctx is cancelled.
func (m *ChildManager) supervisorLoop(ctx context.Context) {
	tick := time.NewTicker(m.opts.SupervisorInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.healthSweep(ctx)
		}
	}
}

func (m *ChildManager) healthSweep(ctx context.Context) {
	m.mu.RLock()
	probes := make([]*Child, 0, len(m.children))
	for _, c := range m.children {
		if c.State() == StateReady {
			probes = append(probes, c)
		}
	}
	m.mu.RUnlock()

	for _, c := range probes {
		pctx, cancel := context.WithTimeout(ctx, defaultPingTimeout)
		err := c.Ping(pctx)
		cancel()
		if err == nil {
			continue
		}
		m.opts.Logger.Printf("mcpbridge: child %q ping failed: %v", c.Name, err)
		m.handleChildDeath(ctx, c, err)
	}
}

func (m *ChildManager) handleChildDeath(ctx context.Context, c *Child, cause error) {
	policy := defaultRestartPolicy
	limit := defaultMaxRestartPerMin
	if c.Cfg.Sandrpod != nil {
		if c.Cfg.Sandrpod.RestartPolicy != "" {
			policy = c.Cfg.Sandrpod.RestartPolicy
		}
		if c.Cfg.Sandrpod.MaxRestartPerMin > 0 {
			limit = c.Cfg.Sandrpod.MaxRestartPerMin
		}
	}

	// Tear down the dead one.
	_ = c.Stop(ctx)
	c.setFailedReason(fmt.Sprintf("died: %v", cause))
	m.rebuildIndexAfterChange()

	switch policy {
	case RestartNever:
		m.opts.Logger.Printf("mcpbridge: %q died, restart_policy=never, leaving down", c.Name)
		return
	case RestartOnFailure, RestartAlways:
		// fall through
	default:
		m.opts.Logger.Printf("mcpbridge: %q unknown restart_policy %q, defaulting to always", c.Name, policy)
	}

	// Rate-limit restarts.
	if !c.recordRestartAttempt(limit) {
		m.opts.Logger.Printf("mcpbridge: %q exceeded %d restarts/min, marking failed", c.Name, limit)
		m.opts.Audit.Record(AuditEvent{
			Source:   "mcp.restart",
			Decision: DecisionDeny,
			Server:   c.Name,
			Reason:   "rate limit exceeded",
		})
		return
	}

	// Exponential backoff before the actual respawn. Without this, a
	// child that crashes on startup (e.g. waiting on a slow API that
	// times out) will burn its entire per-minute restart budget in a
	// fraction of a second. Backoff turns "thrash then give up" into
	// "wait and try again with growing patience".
	backoff := computeBackoff(c.restarts)
	if backoff > 0 {
		m.opts.Logger.Printf("mcpbridge: %q died, waiting %s before restart attempt %d", c.Name, backoff, c.restarts+1)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}

	m.opts.Logger.Printf("mcpbridge: restarting %q (attempt %d)", c.Name, c.restarts+1)
	m.mu.Lock()
	delete(m.children, c.Name)
	m.spawnLocked(ctx, c.Name, c.Cfg)
	// Preserve the restart count across the new Child instance.
	if next, ok := m.children[c.Name]; ok {
		next.mu.Lock()
		next.restarts = c.restarts + 1
		next.restartTimes = c.restartTimes
		next.mu.Unlock()
	}
	m.rebuildIndexLocked()
	m.mu.Unlock()

	m.opts.Audit.Record(AuditEvent{
		Source:   "mcp.restart",
		Decision: DecisionAllow,
		Server:   c.Name,
	})
}

// computeBackoff returns the wait before attempt #(consecutiveFailures+1).
// Doubles per failure, capped at restartBackoffMax. The first failure
// (consecutiveFailures == 0) waits restartBackoffBase.
func computeBackoff(consecutiveFailures int) time.Duration {
	if consecutiveFailures < 0 {
		consecutiveFailures = 0
	}
	d := restartBackoffBase
	for i := 0; i < consecutiveFailures && d < restartBackoffMax; i++ {
		d *= 2
	}
	if d > restartBackoffMax {
		d = restartBackoffMax
	}
	return d
}

func (m *ChildManager) rebuildIndexAfterChange() {
	m.mu.Lock()
	m.rebuildIndexLocked()
	m.mu.Unlock()
}

// startWatcherLocked arms fsnotify on the config file's parent dir.
// Caller holds m.mu.
//
// We watch the DIR rather than the file so that:
//   - atomic-replace saves (editor) don't lose the watcher on inode swap
//   - the file being absent at start time is no obstacle — fsnotify is
//     happy watching an empty dir, and a later Create on the dir fires
//     a normal event
//
// If the parent dir doesn't exist yet, we create it (0700) — this is
// the canonical $HOME/.sandrpod dir that other sandrpod subsystems
// also create on demand.
func (m *ChildManager) startWatcherLocked() error {
	dir := parentDir(m.opts.ConfigPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir watch dir %s: %w", dir, err)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return err
	}
	m.watcher = w
	go m.watchLoop(w)
	return nil
}

func (m *ChildManager) watchLoop(w *fsnotify.Watcher) {
	target := m.opts.ConfigPath
	// Coalesce bursts (atomic save = Rename+Create+Write) into one Reload.
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	// All four ops are interesting:
	//   Write   — in-place edit
	//   Create  — file freshly created (first-time install or post-rm)
	//   Rename  — atomic-save: old inode renamed away, new one appears
	//   Remove  — user deleted the file → Reload sees ENOENT, tears children
	const interestingOps = fsnotify.Write | fsnotify.Create | fsnotify.Rename | fsnotify.Remove
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Name != target {
				continue
			}
			if ev.Op&interestingOps == 0 {
				continue
			}
			debounce.Reset(250 * time.Millisecond)
		case <-debounce.C:
			m.opts.Logger.Printf("mcpbridge: config changed, reloading")
			if err := m.Reload(m.supervisorCtx); err != nil {
				m.opts.Logger.Printf("mcpbridge: reload failed: %v", err)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			m.opts.Logger.Printf("mcpbridge: watcher error: %v", err)
		}
	}
}

func parentDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

// hashServerConfig produces a stable digest of the per-server config used
// by Reload to detect changes worth restarting for.
func hashServerConfig(sc ServerConfig) string {
	type stable struct {
		Command  string
		Args     []string
		EnvKV    [][2]string // sorted
		Sandrpod *SandrpodOpts
	}
	kv := make([][2]string, 0, len(sc.Env))
	for k, v := range sc.Env {
		kv = append(kv, [2]string{k, v})
	}
	sort.Slice(kv, func(i, j int) bool { return kv[i][0] < kv[j][0] })

	b, _ := json.Marshal(stable{
		Command:  sc.Command,
		Args:     sc.Args,
		EnvKV:    kv,
		Sandrpod: sc.Sandrpod,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

const aliasMaxLen = 16

// fullyQualifiedName builds alias__tool, truncating long aliases with a
// content-hash suffix to disambiguate.
func fullyQualifiedName(alias, tool string) string {
	a := alias
	if len(a) > aliasMaxLen {
		a = a[:aliasMaxLen-7] + "_" + shortHash(alias)
	}
	return a + "__" + tool
}

func shortHash(s string) string {
	const (
		offset uint32 = 2166136261
		prime  uint32 = 16777619
	)
	h := offset
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= prime
	}
	const hex = "0123456789abcdef"
	b := make([]byte, 6)
	for i := range 6 {
		b[5-i] = hex[h&0xF]
		h >>= 4
	}
	return string(b)
}

// SplitFQName is exposed for tests / aggregator round-trips.
func SplitFQName(fq string) (alias, tool string, ok bool) {
	return strings.Cut(fq, "__")
}
