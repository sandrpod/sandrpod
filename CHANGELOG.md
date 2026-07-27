# Changelog

All notable changes to SandrPod are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Dates are month-granularity; the project moves in continuous small releases, so entries group by theme.

---

## [Unreleased]

### Added
- **`scripts/cli-sweep.py`** — an acceptance sweep for `sandrpod-cli` against a
  live deployment, shelling out to the real binary so what is tested is what a
  user types. Six commands are
  39 checks covering **all 55 commands** — server, tokens, lifecycle, exec, the
  stateful kernel, sessions, the filesystem, port previews, async jobs, the
  interactive PTY, directory watching, snapshots, MCP and local config. Each
  check declares which commands it exercises, and the run reconciles that
  against `sandrpod-cli --help`, so a command added later shows up as uncovered
  instead of quietly never being run. The sandbox it creates is torn down in a `finally`, so
  an interrupted run — Ctrl-C, or piping the output through `head` — does not
  leave one running, and `snapshot` writes a fixed tag so repeated runs
  overwrite one image instead of accumulating them on the worker.

### Fixed
- **The sandbox image carries GNU grep and findutils**, not just BusyBox's.
  Agent frameworks build GNU-flavoured commands: deepagents' default sandbox
  backend greps with `grep -rHnFZ`, BusyBox rejects `-Z`, and the command is
  wrapped in `2>/dev/null || true` — so the failure was silent and the agent was
  told the file contained nothing rather than that grep had broken. A wrong
  answer delivered confidently is worse than an error.
- **A missing working directory says so.** `os/exec` reports an absent `Dir` as
  ENOENT against the *binary*, so starting a process with a `cwd` that does not
  exist failed with `fork/exec /bin/bash: no such file or directory` — sending
  you to look for a shell that was right there. Now: `working directory
  "/home/user" is not usable: …`. Reached easily: an agent framework whose
  default workdir is `/home/user` hits it on its first call.
- **`sandrpod-cli fs ls` takes its path positionally**, like every other `fs`
  subcommand (`fs cat NAME PATH`, `fs mkdir NAME PATH`, …). It was the one that
  required `--path`, so the obvious `fs ls my-sandbox /workspace` failed with a
  usage error. `--path` still works. Released to PyPI as `sandrpod-cli` 0.2.5.
- **Enabling the E2B surface no longer takes the whole domain.** The host router
  matched `*.<domain>` and handed all of it to the compatibility gateway, so
  with `SANDRPOD_E2B_DOMAIN` set there was no hostname left for the native API —
  `sandrpod-cli`, the REST API and the SDKs 404'd everywhere under the domain
  and could only be reached over an SSH tunnel to the loopback port. The gateway
  now takes the sandbox hosts `<port>-<sandboxID>.<domain>` whole, and on
  `api.<domain>` only its own paths (`/sandboxes`, `/templates`, `/snapshots`,
  `/volumes`) — which do not overlap the native `/api/v1/*`, `/health`,
  `/metrics` and `/ws/*`. Both surfaces therefore share the one hostname, and
  `sandrpod-cli` and the SDKs keep working exactly as before. Compatibility for
  existing E2B users no longer costs you your own API, or a second hostname.

## [0.5.2] — 2026-07

Three E2B-compatibility fixes, all of them only reachable in **domain mode**
(`SANDRPOD_E2B_DOMAIN`). The earlier verification ran against the plain-HTTP
debug listener, where requests never traverse the `<port>-<sandboxID>.<domain>`
host router — so none of these could show up there.

### Fixed
- **`Sandbox.is_running()` always returned `False`.** The SDK probes `GET
  /health` on the envd host and reads 502 as "not running". `/health` is not an
  envd RPC path, so it fell through to the generic port proxy, which dialled
  `127.0.0.1:49983` inside the container, found nothing listening and returned
  502 — for a perfectly healthy sandbox. After `kill()` the probe now returns
  502 rather than 401, so `is_running()` returns `False` instead of raising:
  the per-sandbox envd token dies with the sandbox it names.
- **Code-interpreter contexts were unusable** (`create_code_context`,
  `list_code_contexts`, `restart_code_context`, `remove_code_context` all 401).
  The SDK sends `X-Access-Token` on `/execute` but no credential at all on
  `/contexts`. Accepted now on the code-interpreter port only, for `/contexts`
  only, and only when the sandbox is named by the Host — never by the
  caller-supplied `E2b-Sandbox-Id` header.

### Changed
- **`get_host(port)` URLs are fetchable without an API key**, matching E2B:
  possession of the unguessable `<port>-<sandboxID>.<domain>` hostname is the
  capability. Requiring a header made the common case — opening the dev server
  running in your sandbox, pointing a webhook at it — impossible rather than
  inconvenient, since a browser cannot attach one. `SANDRPOD_E2B_PRIVATE_PORTS=1`
  restores the previous behaviour. The envd RPC surface (filesystem, process,
  PTY) and the code interpreter authenticate either way.

## [0.5.1] — 2026-07

### Fixed
- **The Poder now negotiates the Docker API version with the daemon**
  instead of demanding whatever version its SDK was built against. Against
  any daemon older than the SDK (Docker 25.x and earlier report a maximum
  of API 1.45, while the vendored SDK speaks 1.51) every call failed with
  `client version 1.51 is too new`, so the Poder connected and heartbeated
  but could never list or create a sandbox. `DOCKER_API_VERSION` still
  overrides the negotiated value when it is set.

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
