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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/sandrpod/sandrpod/pkg/audit"
	"github.com/sandrpod/sandrpod/pkg/egress"
	"github.com/sandrpod/sandrpod/pkg/notify"
	"github.com/sandrpod/sandrpod/pkg/permission"
	"github.com/sandrpod/sandrpod/pkg/relay"
	"github.com/sandrpod/sandrpod/pkg/sandbox"
	"github.com/sandrpod/sandrpod/pkg/toolbox"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// agentVersion is overridden at build time via -ldflags "-X main.agentVersion=…".
// Default "dev" makes local builds clearly distinguishable on the server side.
var agentVersion = "dev"

// permissionStore is the store installPermissionGate loaded, so the OS sandbox
// can mask exactly the paths the gate hard-locks without loading it twice.
var permissionStore *permission.Store

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

// envIntOr parses an integer from the environment, falling back to the default
// when the variable is unset or unparsable.
func envIntOr(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
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

	// OS sandbox — the kernel floor under the proxy. Wraps every process the
	// toolbox spawns in bubblewrap (Linux) or Seatbelt (macOS), so a call that
	// ignores the proxy variables still cannot reach the network from outside
	// the work tree, and a hard-locked path is unreadable to open(2), not just
	// to the file API.
	//
	//   off       — no wrapper (default: an OS sandbox that breaks a toolchain
	//               is worse than none, and this one has to be chosen)
	//   auto      — use the platform backend when it works here, report it when
	//               it does not
	//   bwrap     — force bubblewrap; fail startup if it cannot run
	//   seatbelt  — force sandbox-exec; fail startup if it cannot run
	osSandbox = flag.String("os-sandbox", envOr("SANDRPOD_OS_SANDBOX", "off"), "OS sandbox for spawned processes: off | auto | bwrap | seatbelt")
	// How deep below work_dir to look for the write-protected paths (git
	// hooks/config, shell startup files, MCP config, IDE launch config). The
	// scan needs concrete paths to mount over, so it costs a walk of the tree.
	mandatoryDenyDepth = flag.Int("mandatory-deny-depth", envIntOr("SANDRPOD_MANDATORY_DENY_DEPTH", 3),
		"How deep below work_dir to protect against agent-written persistence (0-10)")

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
	// `sandrpod-agent relay …` is the in-sandbox half of the network-isolated
	// mode: the agent binary is bind-mounted into the sandbox read-only and
	// started there to bridge TCP-on-loopback to the proxy's unix socket. It
	// is dispatched before flag.Parse so the relay sees only its own flags.
	if len(os.Args) > 1 && os.Args[1] == "relay" {
		runRelay(os.Args[2:])
		return
	}

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

	// OS sandbox: the layer that covers what the gate and the proxy cannot see.
	if err := installOSSandbox(tb); err != nil {
		log.Fatalf("os sandbox setup failed: %v", err)
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
	permissionStore = store

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
		log.Printf("egress: enforcing from %s (%s)", *egressPolicy, describeEgressListen(srv))
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
		log.Printf("egress: policy from control plane (%s); deny-all until the first sync", describeEgressListen(srv))
		target := egressTarget{apiURL: *apiURL, token: *token, sandbox: *name}
		ctx, cancel := context.WithCancel(context.Background())
		egressSyncStop = cancel
		go syncEgressPolicy(ctx, srv, target, *egressSync)
		return nil

	default:
		return fmt.Errorf("unknown --egress-mode %q (expected off|enforce|server)", mode)
	}
}

// startedEgress is the running proxy, kept so a caller (a test, or a future
// graceful shutdown) can reach it after installEgress returns. One agent hosts
// one sandbox, so there is exactly one.
var startedEgress *egress.Server

// egressSyncStop cancels the control-plane sync loop, if one is running.
var egressSyncStop context.CancelFunc

// egressTarget is what the sync loop needs to find its sandbox, captured once
// so the goroutine never reads the process's flag variables. Reading them in
// the loop would race with anything that re-reads flags at runtime (and did
// race with the tests that restore them).
type egressTarget struct {
	apiURL  string
	token   string
	sandbox string
}

// startEgressServer starts the proxies and hands their address to every
// process the toolbox will spawn.
func startEgressServer(compiled *egress.Compiled) (*egress.Server, error) {
	opts := egress.Options{
		Policy: compiled,
		Sink:   egressAuditSink{rec: sharedAuditRecorder},
	}
	// If the OS sandbox is going to take the network away, the proxy must be
	// reachable over a file instead of a port: nothing on the host's loopback
	// is visible from inside a fresh network namespace.
	if netnsWanted() {
		dir, err := egressSocketDir()
		if err != nil {
			return nil, err
		}
		opts.UnixSocketPath = filepath.Join(dir, "proxy.sock")
	}

	srv, err := egress.Start(opts)
	if err != nil {
		return nil, err
	}
	startedEgress = srv

	if srv.UnixSocket() != "" {
		// Inside the sandbox the proxy is 127.0.0.1:<relay port>, reached
		// through the relay the sandbox wrapper starts.
		env := relayProxyEnv(srv.Token(), relayListenAddr)
		toolbox.SetChildEnv(env)
		egressRelay = &sandbox.Relay{
			Binary:             selfBinary(),
			BinaryInSandbox:    relayBinaryPath,
			SocketDir:          filepath.Dir(srv.UnixSocket()),
			SocketDirInSandbox: relaySocketDir,
			SocketName:         filepath.Base(srv.UnixSocket()),
			Listen:             relayListenAddr,
		}
	} else {
		toolbox.SetChildEnv(srv.ProxyEnv())
	}
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
func syncEgressPolicy(ctx context.Context, srv *egress.Server, target egressTarget, interval time.Duration) {
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	var lastApplied string
	waitingForRegistration := false
	apply := func() (retrySoon bool) {
		policy, err := fetchEgressPolicy(ctx, target)
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
func fetchEgressPolicy(ctx context.Context, target egressTarget) (*egress.Policy, error) {
	base := strings.TrimRight(target.apiURL, "/")
	if base == "" {
		return nil, fmt.Errorf("no -api-url configured")
	}
	url := base + "/api/v1/sandboxes/" + neturl.PathEscape(target.sandbox) + "/egress"
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if target.token != "" {
		req.Header.Set("Authorization", "Bearer "+target.token)
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

// Network-isolated egress: the constants and helpers shared by the proxy side
// and the sandbox side.
const (
	// relayListenAddr is where the relay listens *inside* the sandbox. Proxy
	// variables point here; nothing else in the sandbox opens a port.
	relayListenAddr = "127.0.0.1:3128"
	// relayBinaryPath is where the agent binary appears inside the sandbox.
	relayBinaryPath = "/run/sandrpod/relay"
	// relaySocketDir is where the proxy's socket directory appears inside.
	relaySocketDir = "/run/sandrpod/egress"
)

// egressRelay is set by startEgressServer when the proxy listens on a unix
// socket, and consumed by installOSSandbox to build the sandbox policy.
var egressRelay *sandbox.Relay

// netnsWanted reports whether this run should put sandboxed processes in their
// own network namespace: only Linux, only with bubblewrap actually usable, and
// only when an OS sandbox was asked for. Every other combination keeps the
// proxy on TCP and the network in place.
func netnsWanted() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if strings.ToLower(strings.TrimSpace(*osSandbox)) == "" ||
		strings.ToLower(strings.TrimSpace(*osSandbox)) == string(sandbox.ModeOff) {
		return false
	}
	if _, err := sandbox.Resolve(sandbox.ModeBwrap, "", ""); err != nil {
		return false
	}
	return true
}

// egressSocketDir makes a short, private directory for the proxy socket and
// the relay. Short because a unix socket path has to fit in sun_path (108 bytes
// on Linux), and private because the socket is the proxy: anyone who can open
// it can use the sandbox's allow-list.
func egressSocketDir() (string, error) {
	dir, err := os.MkdirTemp("/tmp", "sandrpod-egress-")
	if err != nil {
		return "", fmt.Errorf("egress socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("egress socket dir: %w", err)
	}
	return dir, nil
}

// relayProxyEnv is the proxy environment a network-isolated sandbox gets: the
// relay's loopback address, never the host's.
func relayProxyEnv(token, listen string) []string {
	url := "http://sandbox:" + token + "@" + listen
	return []string{
		"HTTP_PROXY=" + url, "http_proxy=" + url,
		"HTTPS_PROXY=" + url, "https_proxy=" + url,
		// No ALL_PROXY: SOCKS needs a TCP listener on the host, and this
		// sandbox has no route to one.
		"NODE_USE_ENV_PROXY=1",
	}
}

// describeEgressListen renders where the proxy listens, for the startup log.
func describeEgressListen(srv *egress.Server) string {
	if s := srv.UnixSocket(); s != "" {
		return "unix=" + s + " relay=" + relayListenAddr
	}
	return "http=" + srv.HTTPAddr() + " socks=" + srv.SOCKSAddr()
}

// selfBinary is the path of the running agent binary, which doubles as the
// in-sandbox relay.
func selfBinary() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

// runRelay is the `sandrpod-agent relay` entry point. It runs inside the
// sandbox, has no policy of its own, and exits when its listener dies.
func runRelay(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := fs.String("listen", relayListenAddr, "TCP address to accept proxy connections on (loopback)")
	unix := fs.String("unix", "", "path of the egress proxy's unix socket")
	ready := fs.String("ready", "", "write this file once the listener is up")
	_ = fs.Parse(args)
	if *unix == "" {
		log.Fatalf("relay: --unix is required")
	}
	log.Printf("relay: %s -> %s", *listen, *unix)
	if err := relay.Serve(context.Background(), *listen, *unix, *ready); err != nil {
		log.Fatalf("relay: %v", err)
	}
}

// installOSSandbox wraps every spawned process in the platform's OS sandbox.
//
// This is the layer the other two cannot be: the permission gate covers the
// file API, the egress proxy covers processes that honour HTTP_PROXY, and a
// process that calls open(2) or connect(2) directly — which is the whole point
// of Execute and the PTY — is covered only here. What it enforces is the same
// policy the gate describes, expressed where the kernel can act on it:
// read-only root, writable work_dir, hard-locked paths masked.
//
// Modes are explicit because an OS sandbox that silently does not start is
// indistinguishable from one that is protecting you: `auto` uses the backend
// when it works and says so when it does not, while `bwrap`/`seatbelt` refuse
// to start the agent rather than run unprotected.
func installOSSandbox(tb *toolbox.Server) error {
	mode := sandbox.Mode(strings.ToLower(strings.TrimSpace(*osSandbox)))
	if mode == "" {
		mode = sandbox.ModeOff
	}
	if mode == sandbox.ModeOff {
		return nil
	}

	w, err := sandbox.Resolve(mode, "", "")
	if err != nil {
		return err
	}
	if w == nil {
		// auto, and this host has no backend. Say what is missing rather than
		// let an operator believe something is enforced.
		log.Printf("os sandbox: %s requested but not usable on this host — processes run unwrapped", mode)
		return nil
	}

	workDir := tb.Executor().GetWorkDirForPermission()
	policy := sandboxPolicyFor(workDir, *mandatoryDenyDepth)
	if egressRelay != nil {
		// Egress is enforcing and the proxy is on a unix socket: take the
		// sandbox's network away and let the relay be its only way out.
		policy.NetworkNone = true
		policy.Relay = egressRelay
		log.Printf("os sandbox: network namespace removed — the relay at %s is the only route out", relayListenAddr)
	}
	toolbox.SetSandbox(w, policy)

	log.Printf("os sandbox: %s active — work_dir=%s writable, %d hard-locked path(s) masked, %d persistence path(s) read-only",
		w.Name(), workDir, len(policy.ReadDeny), len(policy.WriteDeny))
	for _, p := range policy.ReadDeny {
		log.Printf("os sandbox:   masked %s", p)
	}
	if workDir == "" {
		log.Printf("os sandbox: WARNING: no work_dir — nothing is writable, commands will fail")
	}
	return nil
}

// sandboxPolicyFor builds the sandbox policy for a work tree: the hardlocks it
// must not read, and the persistence paths it must not write.
//
// The two lists come from the permission store, so the gate and the kernel
// enforce the same policy rather than two that drift.
func sandboxPolicyFor(workDir string, denyDepth int) sandbox.Policy {
	p := sandbox.Policy{WorkDir: workDir, ReadDeny: hardlockPaths()}
	if workDir != "" {
		p.WriteDeny = permission.MandatoryDenyPaths(workDir, denyDepth)
	}
	return p
}

// hardlockPaths returns the hard-locked paths from the permission store, in the
// form the sandbox profile needs (absolute, existing).
func hardlockPaths() []string {
	if permissionStore == nil {
		return nil
	}
	var out []string
	for _, r := range permissionStore.Snapshot().Rules {
		if r.Scope != permission.ScopeHardlock {
			continue
		}
		out = append(out, r.Path)
	}
	return out
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
