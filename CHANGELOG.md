# Changelog

All notable changes to SandrPod are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Dates are month-granularity; the project moves in continuous small releases, so entries group by theme.

---

## [Unreleased]

_Nothing yet._

## [0.5.0] — 2026-07

### Security
- **Sandbox ownership is enforced on the E2B data plane** (envd, code
  interpreter, and the generic port proxy) — previously only the control
  plane checked ownership, so a valid key holder who learned another
  tenant's sandbox ID could reach it. Agent-connect and admin-create now
  stamp `Owner` so no sandbox is world-visible.
- **Platform credentials are stripped before proxying to workers**
  (`X-Sandrpod-Token` always; `Authorization` except on the MCP path,
  which carries the per-sandbox `mcp_token`) — a malicious worker can no
  longer capture the platform admin token from forwarded requests.
- **The tray settings server is token-gated** (per-session token via
  launch URL, constant-time compare) — closes a localhost port-proxy
  pivot from inside a sandbox.
- **`/procmgr/start` and session exec go through the command gate**
  (deny-scan + audit, same as `/process`); `/code-interpreter/execute` is
  audit-only by design (token scanning of arbitrary source both
  false-positives and is trivially bypassed — documented honestly in
  PERMISSION_AND_AUDIT.md).
- **Loud startup warning when authentication is disabled** — a token-less
  server runs as anonymous admin; that mode is now impossible to miss.
- Hardening bundle: permission-gate session-grant leak, `session_id` path
  traversal, OAuth-callback reflected XSS + state-TTL, seeded hardlocks
  for sandrpod's own security state, widened MCP sensitive-tool keywords,
  `/files/find` result caps, loopback-only OAuth callback, Aliyun
  instance-ID JSON escaping.

### Added
- **E2B MCP Gateway compatibility**: in-sandbox `mcp-gateway` shim
  (`:50005`, Streamable-HTTP + Bearer token) plus generic per-port
  subdomain routing (`<port>-<sandbox-id>.<domain>`).
- **Native OAuth for remote MCP servers** (`"auth": "oauth"` in mcp.json):
  child parks in `waiting_auth`, agent opens the system browser, loopback
  callback exchanges the code (PKCE + dynamic client registration), token
  persisted 0600 and auto-refreshed. Verified end-to-end against Notion's
  hosted MCP.
- `sandrpod-cli mcp` command group (`ls` / `add` / `rm` / `url` / `tools`)
  and matching `mcp_*` methods in the Python SDK.
- MCP permission gate improvements: `-mcp-grant-scope server|tool`
  (server-wide grants by default), real session grants, `server:*`
  wildcards, and grants hot-reload — hand edits and revocations apply
  without an agent restart.
- `SANDRPOD_BRAND` env to white-label the tray and consent-dialog strings.

### Fixed
- Agents on non-UTF-8 Windows (GBK `cmd /c ver` output) failed to persist
  on PostgreSQL and appeared OFFLINE; the server now sanitizes runtime
  strings and no longer swallows store errors.
- Windows tray icon rendered empty (placeholder image + Windows' ICO
  requirement); real per-platform icons are embedded now.
- A corrupt `mcp_grants.json` disabled the permission gate open (allow-all);
  it now degrades to prompt-for-everything.
- Release binaries could not report their version (`-X main.version`
  injected into a nonexistent variable); server/agent/poder now carry it,
  and container images receive it as a build arg.
- Chinese Windows agents (GBK `ver` output) failed to persist on
  PostgreSQL and appeared OFFLINE (also listed above — shipped in this
  release).

### Changed
- **LICENSE is now the canonical Apache-2.0 text** (the previous file was
  a paraphrase that license detectors could not recognize); the Python
  packages' metadata says Apache-2.0 accordingly.
- Default container images point at the published `ghcr.io/sandrpod/*`
  names everywhere (code defaults, compose, docs); the release workflow
  also publishes the `server` image, and image builds cross-compile
  instead of running under QEMU.
- Docs accuracy sweep for open-sourcing: E2B compatibility documented as
  shipped/verified, provisioning guides reflect idle reclamation and
  SSH-key persistence, architecture doc brought to v0.4 reality.

## [0.4.0] — 2026-07

### Added
- **E2B wire-protocol compatibility**: the unmodified E2B SDK works as a
  drop-in — control-plane REST, envd filesystem/process surface, stateful
  code-interpreter contexts with chart extraction, PTY, commands
  (background/stream/stdin/kill), snapshots, pause/resume, metrics,
  watch_dir — behind a config-driven provider and per-sandbox subdomain
  routing.
- **PostgreSQL backend + multi-instance LOAD mode**: one dialect-neutral
  SQL store targets SQLite or PostgreSQL from the same code; N active
  server instances share a database, claim jobs via
  `FOR UPDATE SKIP LOCKED`, and forward requests cross-node to the
  instance holding a sandbox's tunnel.
- API token issuance and management (`sandrpod-cli token create/list/rm`,
  hash-at-rest, hot reload), Prometheus `/metrics`.

## [0.3.0] — 2026-06

### Added
- Cloud coverage grew to **8 providers** — AWS, Aliyun, Azure, GCP,
  Tencent, DigitalOcean, Hetzner, Oracle — over two remote-exec backends
  (managed run-command APIs, or SSH with per-VM ephemeral keys).
- Sandbox lifecycle: idle-TTL reclamation, async create with queryable
  jobs, per-sandbox CPU/memory limits, snapshots (`docker commit`),
  preview port proxy, interactive PTY shell through the tunnel.
- TypeScript SDK MVP and web console MVP.

## [0.2.0] — 2026-05

### Added
- **Employee-PC mode**: opt-in permission gate (work_dir → hardlock →
  permanent → session → ask), native consent dialogs on macOS / Linux /
  Windows, NDJSON audit log with at-least-once central upload, and the
  `sandrpod-tray` companion (menu-bar UI, local settings page, IPC over a
  unix socket).
- **MCP transport bridge**: aggregate N stdio/remote MCP servers from a
  standard `mcp.json` into one Streamable-HTTP `/mcp` endpoint — locally
  (`--mcp-only`) or through the sandbox tunnel; hot reload, per-tool
  allow/deny lists, and two-layer auth (platform header + optional
  per-sandbox `mcp_token`).

## [0.1.0] — 2026-04

### Added
- Initial release: API Server control plane; Poder worker managing Docker
  sandbox lifecycles; Toolbox in-sandbox execution service (exec, PTY,
  files, sessions); WebSocket + yamux reverse tunnel (zero inbound ports
  on workers); `sandrpod-agent` direct-machine mode; AWS and Aliyun
  providers; Python SDK (`langchain-sandrpod`) and `sandrpod-cli`.
