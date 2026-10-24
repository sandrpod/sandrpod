// Copyright 2026 SandrPod Contributors
// sandrpod-agent — local machine sandbox agent
//
// Registers the local machine directly as a SandrPod Sandbox without
// requiring a Poder/Docker layer. Connects to the API Server via a
// WebSocket + yamux reverse tunnel and embeds Toolbox to provide
// remote AI agents with code execution capabilities.
//
// Usage:
//
//	sandrpod-agent -api-url=https://your-api-server.com -name=my-laptop -token=<api-key>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/sandrpod/sandrpod/pkg/audit"
	"github.com/sandrpod/sandrpod/pkg/egress"
	"github.com/sandrpod/sandrpod/pkg/notify"
	"github.com/sandrpod/sandrpod/pkg/permission"
	"github.com/sandrpod/sandrpod/pkg/toolbox"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// agentVersion is overridden at build time via -ldflags "-X main.agentVersion=…".
// Default "dev" makes local builds clearly distinguishable on the server side.
var agentVersion = "dev"

// sharedAuditRecorder is set by installAuditPipeline and reused by the MCP
// bridge so both subsystems write through the same Recorder (= same mutex,
// same rotation cadence). Without sharing, two recorders racing on
// active.log can drop events at rotation boundaries.
var sharedAuditRecorder *audit.Recorder

// auditAdapter bridges pkg/audit.Recorder to permission.AuditSink, keeping
// pkg/permission free of any pkg/audit dependency.
type auditAdapter struct {
	rec *audit.Recorder
}

func (a *auditAdapter) Record(source, decision, path, mode, caller, sessionID, reason, matchedCommand string) {
	if a == nil || a.rec == nil {
		return
	}
	if err := a.rec.Record(audit.Event{
		Source:         audit.Source(source),
		Decision:       decision,
		Path:           path,
		Mode:           mode,
		Caller:         caller,
		SessionID:      sessionID,
		Reason:         reason,
		MatchedCommand: matchedCommand,
	}); err != nil {
		// Audit is a compliance control, not best-effort telemetry: a write
		// failure (disk full, dir went read-only) means decisions are no
		// longer being logged. Never swallow it silently — surface it so an
		// operator watching the agent's logs/syslog notices.
		log.Printf("audit: failed to record %s decision for %q: %v", decision, path, err)
	}
}

// envDurationOr parses a duration from the environment, falling back to the
// default when the variable is unset or unparsable.
func envDurationOr(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}

func envOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envBool reads a boolean env var (true/false/1/0/yes/no, case-insensitive),
// returning defaultVal when unset or unparseable.
func envBool(key string, defaultVal bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return defaultVal
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
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

	// Permission gate — opt-in for employee PC mode where the OS user
	// expects desktop consent prompts before AI touches anything outside
	// the sandbox work_dir.
	//
	// Modes:
	//   off     — legacy behavior; only the system-path blacklist applies
	//   prompt  — work_dir is silent-allow, anything else asks the human
	//             via the platform notifier (osascript on macOS, …)
	//   strict  — like prompt, but no notifier is wired (NopNotifier);
	//             everything outside work_dir is silently denied. Useful
	//             for headless setups that intentionally refuse to run
	//             outside the sandbox.
	permissionMode = flag.String("permission-mode", envOr("SANDRPOD_PERMISSION_MODE", "off"), "Permission gate: off | prompt | strict")
	permissionFile = flag.String("permission-file", envOr("SANDRPOD_PERMISSION_FILE", ""), "Override path to permissions.json (default: ~/.sandrpod/permissions.json)")

	// Audit upload — independent of the permission gate so an operator
	// running --permission-mode=off can still get pure observability
	// (every file access logged, nothing blocked). Off by default.
	auditDir         = flag.String("audit-dir", envOr("SANDRPOD_AUDIT_DIR", ""), "Audit log directory (default: ~/.sandrpod/audit). Empty disables local audit.")
	auditUploadURL   = flag.String("audit-upload-url", envOr("SANDRPOD_AUDIT_UPLOAD_URL", ""), "Endpoint to POST audit batches to. Empty disables upload (still logs locally).")
	auditUploadToken = flag.String("audit-upload-token", envOr("SANDRPOD_AUDIT_UPLOAD_TOKEN", ""), "Bearer token sent with audit upload requests. Defaults to -token if empty.")

	// Egress policy — the allow-list every sandboxed process is filtered
	// through (see docs/EGRESS_POLICY.md). Off by default, like the
	// permission gate: turning it on without a policy file would deny every
	// connection, which is a surprise no operator should get from a flag.
	//
	//   off      — no proxy; egress is whatever the host allows
	//   enforce  — start the loopback HTTP+SOCKS5 proxy, hand its address to
	//              every spawned process, and refuse anything the policy or
	//              the resolved-address check does not allow
	egressMode   = flag.String("egress-mode", envOr("SANDRPOD_EGRESS_MODE", "off"), "Egress policy: off | enforce (local file) | server (control plane)")
	egressPolicy = flag.String("egress-policy", envOr("SANDRPOD_EGRESS_POLICY", ""), "Path to the egress policy JSON (required with -egress-mode=enforce)")
	egressSync   = flag.Duration("egress-sync", envDurationOr("SANDRPOD_EGRESS_SYNC", 60*time.Second), "How often -egress-mode=server re-reads the policy from the control plane")

	// MCP transport bridge — see docs/MCP_BRIDGE.md
	mcpEnabled       = flag.Bool("mcp-enabled", envBool("SANDRPOD_MCP_ENABLED", true), "Enable MCP transport bridge if mcp.json is present (default true).")
	mcpConfigPath    = flag.String("mcp-config", envOr("SANDRPOD_MCP_CONFIG", ""), "Path to mcp.json (default: ~/.sandrpod/mcp.json). Empty + missing default disables the bridge.")
	mcpHotReload     = flag.Bool("mcp-hot-reload", envBool("SANDRPOD_MCP_HOT_RELOAD", true), "Watch mcp.json and diff-reload on change.")
	mcpOnly          = flag.Bool("mcp-only", envBool("SANDRPOD_MCP_ONLY", false), "Run only the MCP bridge: no tunnel, no toolbox. Listens on -mcp-listen.")
	mcpListen        = flag.String("mcp-listen", envOr("SANDRPOD_MCP_LISTEN", "127.0.0.1:7090"), "HTTP listen address used in --mcp-only mode.")
	mcpToken         = flag.String("mcp-token", envOr("SANDRPOD_MCP_TOKEN", ""), "Shared secret required on /mcp requests (Authorization: Bearer <token>). Empty disables auth — only safe when the tunnel/listener is itself the trust boundary.")
	mcpGuardManifest = flag.Bool("mcp-guard-manifest", envBool("SANDRPOD_MCP_GUARD_MANIFEST", false), "Also require -mcp-token on /mcp/manifest. Default false: the manifest is read-only metadata (server names/tool counts, no credentials) and stays reachable with platform auth alone.")
	mcpGrantScope    = flag.String("mcp-grant-scope", envOr("SANDRPOD_MCP_GRANT_SCOPE", "server"), "Granularity of dialog-issued MCP call grants: server (an allow covers every non-sensitive tool on that server) | tool (each tool prompts once). Sensitive tools prompt every time in both modes.")

	// Native OAuth for remote MCP servers (mcp.json entries with "auth":
	// "oauth") — browser consent once, token persisted + auto-refreshed.
	// See docs/MCP_AUTH.md.
	mcpOAuth         = flag.Bool("mcp-oauth", envBool("SANDRPOD_MCP_OAUTH", true), "Enable the browser OAuth flow for mcp.json entries with auth=oauth.")
	mcpOAuthCallback = flag.String("mcp-oauth-callback", envOr("SANDRPOD_MCP_OAUTH_CALLBACK", "127.0.0.1:7099"), "Loopback listen address for the OAuth redirect callback.")
	mcpOAuthTokenDir = flag.String("mcp-oauth-token-dir", envOr("SANDRPOD_MCP_OAUTH_TOKEN_DIR", ""), "Directory for persisted OAuth tokens (default: ~/.sandrpod/oauth).")
)

// version is stamped at release time via -ldflags "-X main.version=v…"
// (see .github/workflows/release.yml).
var version = "dev"

func main() {
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	// --mcp-only short-circuits the tunnel/toolbox pipeline: useful for
	// local-LAN MCP aggregation (replaces mcp-proxy/supergateway) and for
	// developer dogfood without an API Server.
	if *mcpOnly {
		runMCPOnly()
		return
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

	log.Printf("Starting SandrPod Agent %s", version)
	log.Printf("Sandbox name: %s", *name)
	log.Printf("API Server:   %s", *apiURL)

	ctx, cancel := context.WithCancel(context.Background())

	// Build the handler once and reuse across reconnects.
	// The toolbox listens only on the yamux tunnel (not a public port),
	// so no separate auth token is needed here — the API Server token
	// already controls tunnel access.
	tb := toolbox.NewServer("", "")

	// If permission gating is requested, install a manager BEFORE accepting
	// any traffic so there is no window where unsanctioned access can slip
	// through during startup.
	if err := installPermissionGate(tb); err != nil {
		log.Fatalf("permission gate setup failed: %v", err)
	}

	// Audit pipeline (recorder + optional uploader). Wired AFTER the
	// permission gate so we can hand the manager the audit sink.
	if err := installAuditPipeline(ctx, tb); err != nil {
		log.Fatalf("audit pipeline setup failed: %v", err)
	}

	// Egress policy. After the audit pipeline (its denials are audited) and
	// before tbHandler(): every process the toolbox spawns from here on
	// inherits the proxy, so there is no window where one runs without it.
	if err := installEgress(); err != nil {
		log.Fatalf("egress policy setup failed: %v", err)
	}

	tbHandler := tb.Handler()

	// Optional MCP transport bridge: aggregates local stdio MCP servers
	// (configured in mcp.json) and exposes them as a single Streamable-HTTP
	// MCP endpoint mounted at /mcp on the agent mux. Off-by-config when no
	// mcp.json is present so users who don't use MCP pay nothing.
	bridgeHandler := installMCPBridge(ctx)

	agentHandler := newAgentMux(tbHandler, *name, bridgeHandler)

	go connectLoop(ctx, agentHandler)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down...")

	// Drain in-flight MCP tool calls before tearing down so we don't
	// leave HTTP clients hanging on a half-served response. The agent
	// tunnel disconnects shortly after we cancel ctx, so 10s is plenty
	// for any in-flight call to either return or be abandoned.
	shutdownMCPBridge(10 * time.Second)
	cancel()
}

// installPermissionGate wires the permission manager into the toolbox's
// embedded executor based on the --permission-mode flag.
//
//   - off:    no-op (legacy behavior).
//   - prompt: load permissions.json, install the platform notifier
//     (osascript modal on macOS).
//   - strict: load permissions.json, install NopNotifier so every path
//     outside work_dir is silently denied.
//
// Any failure (unreadable permissions file, unsupported platform when in
// prompt mode) is fatal — we refuse to start with a broken gate, since
// fail-open is worse than not running.
func installPermissionGate(tb *toolbox.Server) error {
	mode := strings.ToLower(strings.TrimSpace(*permissionMode))
	if mode == "" || mode == "off" {
		return nil
	}

	storePath := *permissionFile
	if storePath == "" {
		def, err := permission.DefaultStorePath()
		if err != nil {
			return fmt.Errorf("locate permissions.json: %w", err)
		}
		storePath = def
	}
	store, err := permission.LoadStore(storePath)
	if err != nil {
		return fmt.Errorf("load permissions.json (%s): %w", storePath, err)
	}

	// Seed baseline protections on first run so `--permission-mode=strict|prompt`
	// never silently runs with zero hardlocks / an empty command policy just
	// because sandrpod-tray was never launched. The agent must not depend on a
	// separate binary for its baseline security posture. Both are no-ops once
	// the store already has entries (the operator's edits are preserved).
	if n, serr := permission.SeedHardlocksIfEmpty(store); serr != nil {
		return fmt.Errorf("seed default hardlocks: %w", serr)
	} else if n > 0 {
		log.Printf("permission gate: seeded %d default hardlock(s) (first run)", n)
	}
	if added, serr := permission.SeedCommandPolicyIfEmpty(store); serr != nil {
		return fmt.Errorf("seed default command policy: %w", serr)
	} else if added {
		log.Printf("permission gate: seeded default command policy (first run)")
	}

	var notifier permission.Notifier
	switch mode {
	case "prompt":
		// Two-tier prompter:
		//
		//   primary  = sandrpod-tray over Unix socket (the user-session GUI)
		//   fallback = in-process MacPrompter (if tray isn't running yet)
		//
		// In production the agent should always be paired with a tray
		// (installed as a LaunchAgent for automatic startup). The fallback
		// is for first-run / dev / "tray crashed and is being restarted"
		// scenarios so a missing tray doesn't fail-close every request and
		// confuse the operator into thinking permission gating is broken.
		//
		// On Linux/Windows the in-process fallback is itself a stub that
		// returns deny-with-error; that's acceptable because we expect the
		// tray to be present on every employee PC, and "unsupported
		// platform" is exactly the error operators need to see.
		sock, err := permission.DefaultSocketPath()
		if err != nil {
			return fmt.Errorf("locate authz socket: %w", err)
		}
		ipcOverride := os.Getenv("SANDRPOD_AUTHZ_SOCKET")
		if ipcOverride != "" {
			sock = ipcOverride
		}
		ipc := permission.NewIPCClient(sock)
		// Fallback to in-process prompter if the tray daemon isn't reachable.
		// notify.NewPrompter() is build-tag-selected: osascript on macOS,
		// zenity/kdialog on Linux, PowerShell MessageBox on Windows.
		ipc.FallbackOnUnavailable = notify.NewPrompter()
		notifier = ipc
	case "strict":
		notifier = permission.NopNotifier{}
	default:
		return fmt.Errorf("unknown --permission-mode %q (expected off|prompt|strict)", mode)
	}

	exec := tb.Executor()
	mgr, err := permission.NewManager(permission.Options{
		Store:    store,
		Notifier: notifier,
		WorkDir:  exec.GetWorkDirForPermission(),
	})
	if err != nil {
		return fmt.Errorf("build permission manager: %w", err)
	}
	exec.SetPermissionManager(mgr)

	workDir := exec.GetWorkDirForPermission()
	log.Printf("Permission gate enabled (mode=%s, store=%s, work_dir=%s)",
		mode, storePath, workDir)

	// A work_dir that swallows hardlocked paths is a policy smell, not a
	// breach: checkBody tests hardlocks first, so ~/.ssh stays denied even
	// when the agent was started from $HOME. Say so anyway — the operator
	// probably meant to scope the agent to a project directory, and they
	// would rather learn it here than from a denial in the audit log.
	if overlapped := permission.HardlocksUnder(workDir, store.Snapshot().Rules, ""); len(overlapped) > 0 {
		log.Printf("WARNING: work_dir %s contains %d hardlocked path(s) (%s); they remain denied — pass -work-dir to scope the agent to a project directory",
			workDir, len(overlapped), strings.Join(overlapped, ", "))
	}
	return nil
}

// installEgress starts the loopback egress proxy and hands its address to
// every process the toolbox spawns.
//
// What this buys: `curl`, `npm`, `pip`, `git` over HTTP(S) and anything else
// that honours the proxy variables can only reach hosts the policy names, and
// the policy names destinations rather than addresses — a permitted name that
// resolves into loopback, the LAN, a cloud metadata endpoint or this host's
// own address is refused at dial time. Denials are audited with the rule that
// caused them, and the refusal the tool sees carries a sentence the model can
// act on.
//
// What this does NOT buy: a process that ignores the proxy variables still
// reaches the network. This layer is a filter, not a wall. The wall is the
// kernel (network namespace for containers, a seatbelt/bwrap profile for
// employee machines) and it is not in this build; docs/EGRESS_POLICY.md is
// explicit about the boundary so nobody assumes more than it delivers.
func installEgress() error {
	mode := strings.ToLower(strings.TrimSpace(*egressMode))
	switch mode {
	case "", "off":
		return nil

	case "enforce":
		// Policy from a local file, fixed for the life of the process.
		if strings.TrimSpace(*egressPolicy) == "" {
			// Starting with no allow list would deny every connection and
			// read, from the outside, exactly like a broken network.
			return fmt.Errorf("--egress-mode=enforce needs --egress-policy <file>; " +
				"an empty allow list denies all egress")
		}
		compiled, err := egress.LoadPolicy(*egressPolicy)
		if err != nil {
			return err
		}
		srv, err := startEgressServer(compiled)
		if err != nil {
			return err
		}
		allow, deny := compiled.Strings()
		log.Printf("egress: enforcing from %s (http=%s socks=%s)", *egressPolicy, srv.HTTPAddr(), srv.SOCKSAddr())
		log.Printf("egress: allow=%v", allow)
		if len(deny) > 0 {
			log.Printf("egress: deny=%v", deny)
		}
		return nil

	case "server":
		// Policy from the control plane, refreshed in the background.
		//
		// It starts with an EMPTY policy — deny all — and only widens once a
		// policy actually arrives. That ordering is the point: the sandbox is
		// reachable through the tunnel from the moment the agent registers,
		// and a proxy that started permissive "for a second" would be a hole
		// with a schedule. A control plane that never answers leaves the
		// sandbox with no egress and a log line saying why, which is the
		// correct failure for a security control.
		empty, err := egress.Parse(egress.Policy{})
		if err != nil {
			return err
		}
		srv, err := startEgressServer(empty)
		if err != nil {
			return err
		}
		log.Printf("egress: policy from control plane (http=%s socks=%s); deny-all until the first sync", srv.HTTPAddr(), srv.SOCKSAddr())
		go syncEgressPolicy(context.Background(), srv)
		return nil

	default:
		return fmt.Errorf("unknown --egress-mode %q (expected off|enforce|server)", mode)
	}
}

// startedEgress is the running proxy, kept so a caller (a test, or a future
// graceful shutdown) can reach it after installEgress returns. One agent hosts
// one sandbox, so there is exactly one.
var startedEgress *egress.Server

// startEgressServer starts the proxies and hands their address to every
// process the toolbox will spawn.
func startEgressServer(compiled *egress.Compiled) (*egress.Server, error) {
	srv, err := egress.Start(egress.Options{
		Policy: compiled,
		Sink:   egressAuditSink{rec: sharedAuditRecorder},
	})
	if err != nil {
		return nil, err
	}
	startedEgress = srv
	toolbox.SetChildEnv(srv.ProxyEnv())
	if sharedAuditRecorder == nil {
		log.Printf("egress: no audit recorder — denials are counted but not persisted")
	}
	return srv, nil
}

// syncEgressPolicy keeps the running proxy in step with the control plane.
//
// Failures are not fatal and do not widen anything: the last policy that
// parsed stays in force until a new one arrives. The comparison is on the
// serialised policy, so a steady state logs nothing every interval.
func syncEgressPolicy(ctx context.Context, srv *egress.Server) {
	interval := *egressSync
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	var lastApplied string
	waitingForRegistration := false
	apply := func() (retrySoon bool) {
		policy, err := fetchEgressPolicy(ctx)
		if errors.Is(err, errSandboxNotRegistered) {
			// The tunnel is still registering the sandbox. Retry on the fast
			// path rather than sitting on deny-all for a whole sync interval.
			if !waitingForRegistration {
				log.Printf("egress: waiting for the control plane to register this sandbox")
				waitingForRegistration = true
			}
			return true
		}
		if err != nil {
			log.Printf("egress: policy sync failed (%v) — keeping the current policy", err)
			return false
		}
		waitingForRegistration = false
		raw, _ := json.Marshal(policy)
		if string(raw) == lastApplied {
			return
		}
		compiled, err := egress.Parse(*policy)
		if err != nil {
			log.Printf("egress: control-plane policy is invalid (%v) — keeping the current policy", err)
			return
		}
		if err := srv.UpdatePolicy(compiled); err != nil {
			log.Printf("egress: apply policy: %v", err)
			return
		}
		lastApplied = string(raw)
		allow, deny := compiled.Strings()
		log.Printf("egress: policy updated from control plane — allow=%v deny=%v", allow, deny)
		return false
	}

	// registrationRetry is the fast path used only while the sandbox is
	// unknown to the control plane; everything after that runs on `interval`.
	const registrationRetry = 2 * time.Second
	next := interval
	if apply() {
		next = registrationRetry
	}
	timer := time.NewTimer(next)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if apply() {
				timer.Reset(registrationRetry)
			} else {
				timer.Reset(interval)
			}
		}
	}
}

// errSandboxNotRegistered reports that the control plane has no record of this
// sandbox yet — which is the normal state for the first seconds of an agent's
// life, since the record appears when the tunnel registers. It is distinct
// from "the sandbox exists but has no policy": one means "ask again shortly",
// the other means "deny everything", and collapsing them costs a minute of
// egress after every agent restart.
var errSandboxNotRegistered = fmt.Errorf("sandbox is not registered with the control plane yet")

// fetchEgressPolicy reads this sandbox's policy from the control plane. A
// sandbox with no policy answers with a null policy, which turns into an empty
// allow-list: "the control plane has nothing to say" and "nothing is allowed"
// are the same answer here, and only one of them is safe.
func fetchEgressPolicy(ctx context.Context) (*egress.Policy, error) {
	base := strings.TrimRight(*apiURL, "/")
	if base == "" {
		return nil, fmt.Errorf("no -api-url configured")
	}
	url := base + "/api/v1/sandboxes/" + neturl.PathEscape(*name) + "/egress"
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errSandboxNotRegistered
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control plane answered %s", resp.Status)
	}
	var body struct {
		Policy *egress.Policy `json:"policy"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}
	if body.Policy == nil {
		return &egress.Policy{}, nil
	}
	return body.Policy, nil
}

// egressAuditSink maps an egress event onto the audit record shape. Denials
// carry the destination, the deciding rule, the reason and the resolved
// address; the model only ever sees the reason, which names the address class
// rather than the address.
type egressAuditSink struct{ rec *audit.Recorder }

func (s egressAuditSink) RecordEgress(e egress.Event) {
	if s.rec == nil {
		return
	}
	decision := "deny"
	if e.Code == egress.CodeAllowed {
		decision = "allow"
	}
	if err := s.rec.Record(audit.Event{
		Source:   audit.SourceEgress,
		Decision: decision,
		Caller:   e.Kind,
		Host:     e.Host,
		Port:     e.Port,
		Code:     e.Code,
		Rule:     e.Rule,
		Reason:   e.Reason,
		Address:  e.Address,
	}); err != nil {
		// Same posture as the permission audit path: a write failure means
		// decisions are no longer being recorded, so say so rather than
		// swallow it.
		log.Printf("audit: failed to record egress %s decision for %s:%d: %v",
			decision, e.Host, e.Port, err)
	}
}

// installAuditPipeline wires the local NDJSON recorder and (if configured)
// the background uploader. The recorder is wired into the permission
// manager only when permission gating is on; we don't want to silently log
// "every file access was allowed" lines when there is no policy at all.
func installAuditPipeline(ctx context.Context, tb *toolbox.Server) error {
	dir := *auditDir
	if dir == "" {
		def, err := audit.DefaultDir()
		if err != nil {
			return fmt.Errorf("locate audit dir: %w", err)
		}
		dir = def
	}
	rec, err := audit.NewRecorder(audit.Options{Dir: dir})
	if err != nil {
		return fmt.Errorf("audit recorder: %w", err)
	}
	log.Printf("audit recorder writing to %s", dir)

	// Stash the recorder so the MCP bridge can reuse it (avoiding two
	// concurrent writers to active.log racing on rotation). Set BEFORE
	// the bridge starts in main().
	sharedAuditRecorder = rec

	// Hand the recorder to the permission manager (if any).
	if mgr := tb.Executor().PermissionManager(); mgr != nil {
		mgr.SetAuditSink(&auditAdapter{rec: rec})
	}

	// Upload is optional. If URL is empty we still log locally — useful
	// for forensic-only deployments where ops will pull the files manually.
	if *auditUploadURL == "" {
		log.Printf("audit upload disabled (no --audit-upload-url); events will accumulate in %s", dir)
		return nil
	}
	tok := *auditUploadToken
	if tok == "" {
		tok = *token
	}
	uploader, err := audit.NewUploader(audit.UploaderOptions{
		URL:          *auditUploadURL,
		Token:        tok,
		Recorder:     rec,
		AgentVersion: agentVersion,
		SandboxName:  *name,
		HostOS:       runtime.GOOS,
		HostArch:     runtime.GOARCH,
	})
	if err != nil {
		return fmt.Errorf("audit uploader: %w", err)
	}
	if uploader != nil {
		uploader.Start(ctx)
		log.Printf("audit upload enabled — POST → %s every 30s (batch size 200)", *auditUploadURL)
	}
	return nil
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
func newAgentMux(tb http.Handler, sandboxName string, mcpBridge http.Handler) http.Handler {
	mux := http.NewServeMux()

	// Mount the MCP bridge first if configured. It owns "/mcp" and any
	// "/mcp/..." subpath (manifest, future SSE-only mode, etc.).
	if mcpBridge != nil {
		mux.Handle("/mcp", mcpBridge)
		mux.Handle("/mcp/", mcpBridge)
	}

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

// getOSVersion is provided by os_version_{unix,windows}.go.
