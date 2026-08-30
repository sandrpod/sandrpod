# SandrPod Architecture

> **Version**: v0.5.8
> **Updated**: 2026-08
> 中文版：[ARCHITECTURE.zh.md](ARCHITECTURE.zh.md)
>
> This document describes the architecture as **implemented and running**.
> Historical design notes live in [`design/architecture-v1.md`](design/architecture-v1.md);
> multi-instance deployment in [`MULTI_INSTANCE_DEPLOYMENT.md`](MULTI_INSTANCE_DEPLOYMENT.md).

---

## 1. Overview

SandrPod is self-hosted execution infrastructure for AI agents. The design rests
on four ideas:

- **The API Server is the only control plane and the only request proxy.**
  Clients talk to it and nothing else.
- **Workers dial out.** A Poder opens a WebSocket reverse tunnel to the API
  Server; the server pushes requests down it. No worker exposes an inbound port.
- **sandrpod-agent turns any machine into a sandbox** without Poder or Docker —
  it registers itself directly and embeds the Toolbox.
- **Toolbox runs inside each sandbox** and provides the code-execution HTTP API.

```
Client / SDK / CLI                    E2B SDK (unmodified)
        │  HTTP                              │  HTTPS
        ▼                                    ▼
┌──────────────────────────────────────────────────────────┐
│                     API Server  :8080                    │
│                                                          │
│  ┌────────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ Control Plane  │  │ Tunnel Proxy │  │ E2B Gateway  │  │
│  │ sandbox CRUD   │  │ execute      │  │ api.<domain> │  │
│  │ jobs, poders   │  │ stream, PTY  │  │ <port>-<id>. │  │
│  │ tokens         │  │ files        │  │   <domain>   │  │
│  └────────────────┘  └──────────────┘  └──────────────┘  │
│                                                          │
│  ┌─────────────────┐  ┌────────────────────┐             │
│  │  tunnelStore    │  │  directStore       │             │
│  │  poderID→tunnel │  │  sandboxName→tunnel│             │
│  └─────────────────┘  └────────────────────┘             │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │  Store — memory / SQLite / PostgreSQL              │  │
│  │  Sandbox · Poder · Job · Token · TunnelOwner       │  │
│  └────────────────────────────────────────────────────┘  │
└────────────┬──────────────────────────┬──────────────────┘
             │ WebSocket + yamux        │ WebSocket + yamux
             ▼                          ▼
   ┌──────────────────┐      ┌───────────────────────────┐
   │  Poder           │      │  sandrpod-agent           │
   │  (Docker worker) │      │  (the machine IS the box) │
   │                  │      │  embedded Toolbox         │
   │  ┌────────────┐  │      │  permission gate + audit  │
   │  │  Toolbox   │  │      └─────────────┬─────────────┘
   │  │ (container)│  │                    │ unix socket
   │  └────────────┘  │      ┌─────────────▼─────────────┐
   └──────────────────┘      │  sandrpod-tray            │
                             │  (user-session GUI)       │
                             └───────────────────────────┘
```

---

## 2. Components

### 2.1 API Server (`cmd/server`)

**Responsibilities**

- Control plane: CRUD for sandboxes, poders, jobs, and API tokens
- Tunnel management: accept WebSocket connections from Poders and Agents,
  maintain the yamux multiplexed sessions
- Request proxying: every execute / file / PTY request travels through a tunnel
- E2B-compatible gateway (optional, host-routed — see §2.6)

**Flags**

| Flag | Default | What it does |
|---|---|---|
| `-port` | `8080` | Listen port |
| `-token` | `""` | API token. Empty = **no auth, anonymous admin** — never expose that |
| `-tokens-file` | `""` | Named-token JSON, hot-reloaded (see `AUTH_AND_KEYS.md`) |
| `-db` | `""` | Storage: empty = memory; `sqlite:<path>`; `postgres://…` |
| `-public-url` | `""` | Public address cloud VMs call back to (required for cloud providers) |
| `-node-url` | `""` | This instance's internal address in multi-instance mode |
| `-tls-cert` / `-tls-key` | `""` | Built-in TLS (listen HTTPS directly) |
| `-rate-limit` | `0` | Requests/second per user token (0 = unlimited) |
| `-max-sandboxes-per-owner` | `0` | Concurrent sandbox cap per owner (0 = unlimited) |
| `-offline-timeout` | `30s` | Mark a Poder OFFLINE after this heartbeat gap |
| `-reap-timeout` | `10m` | Reclaim an OFFLINE poder (terminates the cloud VM) |
| `-sandbox-idle-timeout` | `0` | Reap sandboxes idle longer than this (0 = disabled) |
| `-poder-idle-timeout` | `0` | Reclaim cloud poders with no sandboxes (0 = disabled) |

Every flag also reads an env var (`SANDRPOD_RATE_LIMIT`,
`SANDRPOD_SANDBOX_IDLE_TIMEOUT`, …) as its default. The E2B gateway is
configured purely by env (`SANDRPOD_E2B_DOMAIN` et al). Full list in
`.env.example`.

**WebSocket endpoints**

| Endpoint | Purpose |
|---|---|
| `GET /ws/poder/connect` | Poder dials in, registers, establishes the yamux tunnel |
| `GET /ws/sandbox/connect` | sandrpod-agent dials in, registers itself as a Sandbox |

**HTTP API**

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/health` | Health check (also reports version) |
| `GET` | `/metrics` | Prometheus metrics (admin) |
| `GET` | `/api/v1/poders` | List Poders |
| `GET/DELETE` | `/api/v1/poders/{id}` | Get / delete a Poder |
| `POST` | `/api/v1/sandboxes` | Create a sandbox (enqueues a Job) |
| `GET` | `/api/v1/sandboxes` | List sandboxes |
| `GET/DELETE/PATCH` | `/api/v1/sandboxes/{name}` | Get / delete / update |
| `POST` | `/api/v1/sandboxes/{name}/start\|stop` | Lifecycle |
| `POST` | `/api/v1/sandboxes/execute` | Execute code (proxied to Toolbox) |
| `GET/POST` | `/api/v1/sandboxes/stream` | Streaming execution (SSE) |
| `GET` | `/api/v1/sandboxes/{name}/toolbox/*` | Generic Toolbox proxy (files, PTY, sessions, watch) |
| `GET` | `/api/v1/jobs/poll` | Poder polls for pending Jobs |
| `PATCH` | `/api/v1/jobs/{id}` | Poder reports Job outcome |
| `GET/POST/DELETE` | `/api/v1/tokens…` | API token issuance and revocation (admin) |

---

### 2.2 Poder (`cmd/poder`)

**Responsibilities**

- Dial the API Server over WebSocket and establish the yamux reverse tunnel
- Serve HTTP over that tunnel: forward execute / file / PTY requests to the
  Toolbox inside the target container
- Poll `/api/v1/jobs/poll` and run CREATE / DELETE / START / STOP jobs
- Heartbeat host and container usage

**Flags**

| Flag | Env | Default | What it does |
|---|---|---|---|
| `-api-url` | `API_URL` | `http://localhost:8080` | API Server address |
| `-region` | `REGION` | `local` | Region label used by the scheduler |
| `-provider-type` | `PROVIDER_TYPE` | `local` | `local` / `docker` / a cloud name |
| `-poder-id` | `PODER_ID` | auto | Unique ID (`poder-<container-id-prefix>`) |
| `-network` | `SANDRPOD_NETWORK` | `""` | Docker network to attach sandboxes to |
| `-token` | `SANDRPOD_TOKEN` | `""` | API token |
| `-vm-instance-id` | `VM_INSTANCE_ID` | `""` | Cloud instance ID, for VM reclamation |
| `-heartbeat-interval` | — | `10s` | Heartbeat period |

**A Poder listens on no port at all.** Everything goes through the tunnel it
dialed out.

```bash
docker run -d --name sandrpod-poder \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --add-host host.docker.internal:host-gateway \
  ghcr.io/sandrpod/poder:latest \
  -api-url=http://host.docker.internal:8080 -region=local
```

---

### 2.3 sandrpod-agent (`cmd/agent`)

Registers **the local machine** as a sandbox — no Poder, no Docker. It embeds
the Toolbox and exposes it through a reverse tunnel. This is the employee-PC
path, so it also carries the permission gate and the audit pipeline (§2.7).

**Flags**

| Flag | Env | Default | What it does |
|---|---|---|---|
| `-api-url` | `SANDRPOD_API_URL` | `http://localhost:8080` | API Server address |
| `-name` | `SANDRPOD_SANDBOX_NAME` | — | Sandbox name (globally unique) |
| `-work-dir` | `SANDRPOD_WORK_DIR` | cwd | Execution working directory |
| `-token` | `SANDRPOD_TOKEN` | `""` | API token |
| `-reconnect` | — | `5s` | Reconnect delay |
| `-permission-mode` | `SANDRPOD_PERMISSION_MODE` | `off` | `off` / `prompt` / `strict` |
| `-permission-file` | `SANDRPOD_PERMISSION_FILE` | `~/.sandrpod/permissions.json` | Rule store path |
| `-audit-dir` | `SANDRPOD_AUDIT_DIR` | `""` | Local NDJSON audit log dir (empty = disabled) |
| `-audit-upload-url` | `SANDRPOD_AUDIT_UPLOAD_URL` | `""` | Batch upload endpoint |
| `-audit-upload-token` | `SANDRPOD_AUDIT_UPLOAD_TOKEN` | falls back to `-token` | Bearer token for upload |
| `-mcp-enabled` | `SANDRPOD_MCP_ENABLED` | `false` | Enable the MCP bridge (§2.8) |
| `-mcp-config` | `SANDRPOD_MCP_CONFIG` | `~/.sandrpod/mcp.json` | MCP server config |
| `-mcp-listen` | `SANDRPOD_MCP_LISTEN` | `127.0.0.1:7090` | MCP bridge listen address |
| `-mcp-token` | `SANDRPOD_MCP_TOKEN` | `""` | Bearer token for the MCP endpoint |
| `-mcp-only` | — | `false` | Run only the MCP bridge, skip sandbox registration |
| `-mcp-oauth` | — | `false` | Enable OAuth for MCP servers that require it |
| `-mcp-oauth-callback` | `SANDRPOD_MCP_OAUTH_CALLBACK` | `127.0.0.1:7099` | OAuth redirect listener |
| `-mcp-oauth-token-dir` | `SANDRPOD_MCP_OAUTH_TOKEN_DIR` | `~/.sandrpod/mcp-tokens` | Token storage |
| `-mcp-grant-scope` | `SANDRPOD_MCP_GRANT_SCOPE` | `server` | Consent granularity: `server` / `tool` |
| `-mcp-guard-manifest` | — | `false` | Reject tool-definition drift after approval |
| `-mcp-hot-reload` | — | `false` | Watch the MCP config and reload on change |

**The registered sandbox**

| Field | Value |
|---|---|
| `provider_type` | `local-agent` |
| `proxy_url` | `direct://<sandbox-name>` |
| `state` | `RUNNING` while connected, `ERROR` when disconnected |
| start / stop | **unsupported** — lifecycle belongs to the agent process |

---

### 2.4 Toolbox (`cmd/toolbox`)

Runs inside every sandbox and provides the execution API. Written in Go.
Clients never reach it directly — Poder or Agent proxies to it.

| Group | Paths |
|---|---|
| Execute | `POST /process` · `GET/POST /stream` |
| Stateful sessions | `POST /process/session` · `/process/session/{id}` |
| Code interpreter | `/code-interpreter/execute` · `/code-interpreter/contexts` · `/code-interpreter/contexts/{id}` |
| Process manager | `/procmgr/start` · `/stream` · `/input` · `/stdin-close` · `/signal` · `/resize` · `/list` |
| PTY | `GET /pty/` (WebSocket) · `POST /pty/create` |
| Files | `/files` · `/upload` · `/download` · `/bulk-upload` · `/delete` · `/move` · `/folder` · `/info` · `/find` · `/search` · `/replace` · `/permissions` · `/work-dir` · `/project-dir` · `/user-home-dir` |
| Directory watch | `/watch/create` · `/watch/events` · `/watch/remove` |
| Port proxy | `/proxy/<port>/*` → `127.0.0.1:<port>` inside the sandbox |
| MCP | `/mcp` · `/mcp/*` |
| Introspection | `/health` · `/status` · `/info` · `/metrics` |

The **code interpreter** is the Jupyter-style surface: a context is a persistent
kernel namespace, so variables survive across calls. The **port proxy** is what
makes a dev server inside a sandbox reachable from outside.

---

### 2.5 Storage (`pkg/store`)

Repository pattern; interfaces in `pkg/sandpod/repo.go`:

```
sandpod.SandboxRepository
sandpod.PoderRepository
sandpod.JobRepository
sandpod.TokenRepository
sandpod.TunnelOwnerRepository
sandpod.Stores            ← the aggregate injected into every handler
```

| Package | Backend | Use for |
|---|---|---|
| `pkg/store/memory.go` | in-process maps under an RWMutex | dev / test; lost on restart |
| `pkg/store/sqldb/` | dialect-parameterised SQL: SQLite (WAL, `modernc.org/sqlite`) **or** PostgreSQL (pgx pool, `FOR UPDATE SKIP LOCKED` job claim) | production |

One codebase targets both. `sqldb/dialect.go` handles placeholder rebinding,
DDL types, and the concurrent job claim; a server instance picks exactly one
backend at startup from the `-db` DSN scheme.

```bash
go run ./cmd/server                                    # memory
go run ./cmd/server -db sqlite:./data/sandrpod.db      # SQLite, single instance
go run ./cmd/server -db "postgres://…?sslmode=require" # PostgreSQL, multi-instance
```

`TunnelOwnerRepository` is what makes multi-instance work: it records which node
holds each sandbox's tunnel, so a request landing anywhere can be forwarded to
the right node. See [`MULTI_INSTANCE_DEPLOYMENT.md`](MULTI_INSTANCE_DEPLOYMENT.md).

---

### 2.6 E2B-compatible gateway (`pkg/e2bcompat`)

Lets an **unmodified E2B SDK** run against SandrPod: point `E2B_DOMAIN` at your
deployment and `Sandbox.create()` works. Off unless `SANDRPOD_E2B_DOMAIN` is set.

Routing is by Host, decided in `cmd/server/e2bgateway.go`:

| Host | Handled by |
|---|---|
| `api.<domain>` + an E2B control-plane path (`/sandboxes`, `/templates`, `/snapshots`, `/volumes`) | E2B control plane |
| `api.<domain>`, anything else | SandrPod's own REST API |
| `<port>-<sandboxID>.<domain>` | E2B data plane |

That split matters: `api.<domain>` is **shared**. Only the four E2B namespaces
are diverted, so `sandrpod-cli` and the native SDKs keep working on the same
hostname with the E2B surface enabled.

Three data-plane surfaces sit behind `<port>-<sandboxID>.<domain>`. Which one
serves a request is decided by **path first, port second**:

- **envd** — filesystem and process RPCs (Connect/gRPC over protobuf), matched
  on the path prefix: `/filesystem.*`, `/process.*`, `/files`. The port in the
  hostname is not consulted; E2B's SDK puts `49983` there, but nothing listens
  on that port inside the sandbox.
- **code interpreter** — `run_code`, matched on `/execute` and `/contexts*`, or
  on the hostname port equal to `CodePort` (default `49999`). Backed by Toolbox
  contexts.
- **generic port proxy** — anything else with a port in the hostname:
  reverse-proxied through the tunnel to Toolbox `/proxy/<port>/`, which dials
  `127.0.0.1:<port>` inside the sandbox. This is how in-sandbox services are
  reached — the MCP gateway on `50005`, a user's dev server, a webhook target.

Two authorization details worth knowing:

- `Config.Authorize` gates the **data plane only**. The control plane reads the
  shared store and is filtered by owner; the data plane derives its target
  sandbox from a caller-supplied Host or header, so without this check any
  authenticated caller could reach another tenant's sandbox by ID.
- `Config.PrivateSandboxPorts` defaults to **false**, matching E2B: possession
  of the unguessable `<port>-<sandboxID>.<domain>` hostname *is* the capability.
  A browser cannot attach an Authorization header, so requiring one would not
  make the common case inconvenient — it would make it impossible. Set it when
  every consumer is a program that can carry the key.

Details and the verified feature matrix: [`E2B_COMPAT.md`](E2B_COMPAT.md).

---

### 2.7 Permission gate and audit (`pkg/permission`, `pkg/notify`, `pkg/audit`)

Opt-in, for the employee-PC deployment where the sandbox *is* someone's laptop.
Enabled with `-permission-mode`; `off` by default, so nothing changes for server
deployments.

**Five-branch decision** (`pkg/permission/manager.go`), first match wins:

1. **work_dir** — inside the agent's working directory, allow silently
2. **hardlock** — deny. Checked *before* any allow-rule lookup, so an
   accidentally-permanent rule on `~/.ssh` can never take effect
3. **permanent rule** — a standing grant the user added
4. **session grant** — a time-limited grant with a TTL
5. **ask the human** — native dialog, and persist the answer if the user chose
   "always"

`pkg/notify` renders that dialog per platform — macOS `osascript`, Linux
`zenity`/`kdialog`, Windows PowerShell `MessageBox` — and **fails closed**:
timeout or error means deny.

`pkg/audit` writes every decision to a local NDJSON log (auto-rotating at 8 MiB)
and, when an upload URL is configured, ships batches to a central endpoint with
at-least-once delivery. It is decoupled from `pkg/permission` through the
`AuditSink` interface.

**sandrpod-tray** (`cmd/sandrpod-tray`) is the user-session companion: tray
icon, the consent dialogs, and a local settings page. It talks to the agent over
`~/.sandrpod/authz.sock`.

```bash
sandrpod-tray serve                                # tray + IPC + settings HTTP
sandrpod-tray rules ls                             # permanent rules and hardlocks
sandrpod-tray rules add ~/Documents --mode rw
sandrpod-tray policy ls                            # command deny/warn lists
sandrpod-tray unlock ~/.ssh --i-understand-the-risk  # CLI only, never from the GUI
sandrpod-tray seed                                 # install default hardlocks
```

Full treatment: [`PERMISSION_AND_AUDIT.md`](PERMISSION_AND_AUDIT.md).

---

### 2.8 MCP bridge (`pkg/mcpbridge`, `cmd/mcp-gateway`)

Aggregates several MCP servers behind one Streamable-HTTP endpoint, so an agent
gets a single tool namespace instead of N connections. `pkg/mcpbridge` supervises
the child servers (spawn, backoff, restart) and merges their tool lists with
`SplitFQName`-style prefixing.

Two deployment shapes:

- **In the agent** — `sandrpod-agent -mcp-enabled`, serving on `127.0.0.1:7090`.
  Child servers run on the user's machine with their credentials, and the
  permission gate applies through `PermissionGate`.
- **In a sandbox** — `cmd/mcp-gateway` runs inside the container on port
  `50005`, reached through the E2B port proxy. Accepts both the E2B `mcp` map
  shape and the sandrpod `{mcpServers:{…}}` shape.

`-mcp-oauth` handles servers requiring OAuth (Notion, etc.): the authorization
URL is only ever surfaced over the admin socket, never to a remote caller.
`-mcp-guard-manifest` pins tool definitions after approval, so a server that
silently changes a tool's schema is rejected rather than trusted.

The bridge is also served on an AF_UNIX socket at
`~/.sandrpod/mcp-local.sock`, sharing the same manager — so a host on this
machine reaches it directly instead of leaving the machine and coming back
through the control plane (0.13 ms rather than ~1 s). Same alias namespace,
same permission gate, same audit: a shortcut, not a bypass. The auth boundary
is the socket's 0600 mode, which is why there is deliberately no token.

**Resources** are proxied alongside tools, which is what lets an
[MCP Apps](https://modelcontextprotocol.io/seps/1865-mcp-apps-interactive-user-interfaces-for-mcp)
host fetch a server's interface HTML: that is only reachable through
`resources/read`, and the CSP and permission declarations governing the iframe
ride on the resource's `_meta`, so the bridge returns upstream contents
untouched. URIs are namespaced into the authority (`ui://form` →
`ui://<alias>/form`) because two servers exposing `ui://form` would otherwise
collide, and each tool's `_meta.ui.resourceUri` is rewritten to match. Servers
that expose no resources are never asked.

See [`MCP_BRIDGE.md`](MCP_BRIDGE.md) and [`MCP_AUTH.md`](MCP_AUTH.md).

---

## 3. State machine

```
PENDING → STARTING → RUNNING → STOPPING → STOPPED
                        │                    │
                        └──────► ERROR ◄─────┘
                                   │
                                   ▼
                              TERMINATED
```

Defined in `pkg/sandpod/interface.go`. `TERMINATED` is permanent. An
agent-registered sandbox only ever occupies `RUNNING` and `ERROR`.

---

## 4. Key flows

### 4.1 Poder registration and tunnel setup

```
Poder                                    API Server
  │                                           │
  │── WS GET /ws/poder/connect ──────────────▶│
  │   Header: X-Poder-ID, X-Poder-Region,     │
  │           X-CPU-Cores, X-Memory-Bytes…    │
  │                                           │
  │◀─────────── 101 Switching Protocols ──────│
  │                                           │
  │◀══════════ yamux session (bidi) ═════════▶│
  │                                           │
  │  Poder is the yamux *server*,             │  server stores the tunnel in
  │  serving HTTP over the session            │  tunnelStore[poderID]
  │                                           │
  ├── PUT /api/v1/poders/{id}/heartbeat ─────▶│  update last_heartbeat + usage
  │   (every 10s, its own HTTP request)       │
  │                                           │
  ├── GET /api/v1/jobs/poll ─────────────────▶│
  │◀────────────── [{job}, {job}] ────────────│
  │                                           │
  │  run the job (CREATE/DELETE/START/STOP)   │
  │                                           │
  ├── PATCH /api/v1/jobs/{id} ───────────────▶│  update job + sandbox state
```

### 4.2 Creating a sandbox (Poder path)

```
Client          API Server          Poder           Docker
  │                 │                 │               │
  │ POST /sandboxes │                 │               │
  ├────────────────▶│                 │               │
  │                 │ SelectBest()    │               │
  │                 │ write Job(PENDING)              │
  │◀── 202 job_id ──│                 │               │
  │                 │                 │               │
  │                 │◀── PollJobs ────│               │
  │                 │─── [{job}] ────▶│               │
  │                 │                 │ docker run    │
  │                 │                 ├──────────────▶│
  │                 │                 │◀── container ─│
  │                 │ PATCH job       │               │
  │                 │◀────────────────│               │
  │                 │ sandbox.State = RUNNING         │
  │ GET /sandboxes/{name}             │               │
  ├────────────────▶│                 │               │
  │◀── {state:RUNNING, ip:…} ─────────│               │
```

### 4.3 sandrpod-agent registration (direct path)

```
sandrpod-agent                       API Server
       │                                  │
       │── WS GET /ws/sandbox/connect ───▶│
       │   Header: X-Sandbox-Name,        │
       │           X-Sandbox-Arch/OS      │
       │                                  │
       │◀──────── 101 Switching ──────────│
       │                                  │
       │◀═══ yamux session ══════════════▶│ directStore[name] = tunnel
       │  agent is the yamux server,      │ store.Add({name, state:RUNNING,
       │  serving the toolbox HTTP API    │   provider_type:"local-agent",
       │                                  │   proxy_url:"direct://name"})
```

### 4.4 Executing code (the common proxy path)

```
Client          API Server                  Poder/Agent    Toolbox
  │                 │                           │             │
  │ POST /execute   │                           │             │
  │ ?sandbox=foo    │                           │             │
  ├────────────────▶│ sandboxTunnel("foo")      │             │
  │                 │ → look up sandbox         │             │
  │                 │ → switch on proxy_url     │             │
  │                 │   tunnel:// → tunnelStore │             │
  │                 │   direct:// → directStore │             │
  │                 │                           │             │
  │                 │── yamux.Open() ──────────▶│             │
  │                 │── HTTP POST /execute ────▶│             │
  │                 │                           │ POST /process
  │                 │                           ├────────────▶│
  │                 │                           │◀── output ──│
  │◀────── output ──│◀──────────────────────────│             │
```

### 4.5 An E2B SDK request

```
e2b.Sandbox                    API Server                  Toolbox
  │                                │                          │
  │ POST api.<domain>/sandboxes    │                          │
  ├───────────────────────────────▶│ host==api && E2B path    │
  │                                │ → E2B control plane      │
  │◀── {sandboxID, …} ─────────────│ → create via scheduler   │
  │                                │                          │
  │ POST 49999-<id>.<domain>/execute                          │
  ├───────────────────────────────▶│ host matches envd pattern│
  │                                │ → resolve sandbox from   │
  │                                │   Host, Authorize()      │
  │                                │ → tunnel                 │
  │                                ├─────────────────────────▶│
  │                                │   /code-interpreter/execute
  │◀────── streamed results ───────│◀─────────────────────────│
```

---

## 5. Repository layout

```
sandrpod/
├── cmd/
│   ├── server/          # API Server: control plane, tunnel proxy, E2B gateway
│   ├── poder/           # Worker: Docker lifecycle + job polling
│   ├── agent/           # Direct-registration agent (the machine is the sandbox)
│   ├── toolbox/         # In-sandbox execution service
│   ├── sandrpod-tray/   # User-session GUI companion (CGO: Cocoa/GTK/win32)
│   └── mcp-gateway/     # In-sandbox MCP aggregator (port 50005)
│
├── pkg/
│   ├── sandpod/         # Core domain
│   │   ├── interface.go     # SandboxInfo, PoderInfo, Job, State
│   │   ├── repo.go          # Repository interfaces + Stores aggregate
│   │   ├── scheduler.go     # Poder selection (SelectBest)
│   │   └── *_store.go       # In-memory stores (legacy; wrapped by pkg/store)
│   │
│   ├── store/           # Repository implementations
│   │   ├── memory.go        # In-memory adapter
│   │   └── sqldb/           # SQLite *or* PostgreSQL from one codebase
│   │       ├── dialect.go       # Placeholder rebinding, DDL types, job claim
│   │       ├── db.go            # Open(), pragmas, startup recovery
│   │       ├── schema.go        # DDL + Migrate()
│   │       ├── sandbox_repo.go  poder_repo.go  job_repo.go
│   │       ├── token_repo.go    # API tokens (hash at rest)
│   │       └── tunnelowner_repo.go  # Which node holds which tunnel
│   │
│   ├── e2bcompat/       # E2B wire-protocol gateway
│   │   ├── gateway.go       # Config, Handler, host routing, authorization
│   │   ├── controlplane.go  # /sandboxes, /templates, /snapshots, /volumes
│   │   ├── envd.go          # Filesystem + process RPCs (Connect/gRPC)
│   │   ├── process.go  protobuf.go  protobuf_process.go
│   │   ├── codeinterp.go    # run_code surface
│   │   ├── watch.go         # Directory watch streaming
│   │   └── apikey.go        # e2b_<hex> key generation and lookup
│   │
│   ├── permission/      # Employee-PC decision engine (5-branch policy)
│   ├── notify/          # Native consent dialogs; fail-closed
│   │   └── prompt_{darwin,linux,windows}.go
│   ├── audit/           # NDJSON recorder + background batch uploader
│   ├── mcpbridge/       # MCP child supervision, tool aggregation, OAuth
│   │
│   ├── poder/           # Pod executor interface + Docker implementation
│   ├── provider/        # Cloud abstraction
│   │   ├── interface.go  factory.go
│   │   ├── aws/ aliyun/ azure/ tencent/ oracle/   # managed run-command
│   │   ├── gcp/ digitalocean/ hetzner/            # SSH
│   │   └── sshexec/         # Shared SSH executor (DO/Hetzner)
│   │
│   ├── toolbox/         # In-sandbox HTTP service
│   │   ├── api.go  executor.go  files.go  pty_unix.go
│   │   ├── procmgr.go       # Process manager
│   │   ├── session*.go      # Stateful sessions
│   │   └── watch.go         # Directory watch
│   │
│   ├── tunnel/          # WebSocket + yamux reverse tunnel
│   ├── logging/  brand/  homedir/                 # Shared infrastructure
│   └── sdk/python/      # Python SDK + sandrpod-cli + langchain-sandrpod
│
├── docker/              # Dockerfiles + reference compose
└── docs/                # This document; index in docs/README.md
```

---

## 6. Ports and environment

| Component | Port | Notes |
|---|---|---|
| API Server | `:8080` | The only port exposed to clients |
| Poder | none | Dials out; listens on nothing |
| sandrpod-agent | none | Same |
| Toolbox | `:8080` in-container | Reached only through a tunnel (`:18080` in tests) |
| MCP bridge (agent) | `127.0.0.1:7090` | Loopback only |
| MCP local socket | `~/.sandrpod/mcp-local.sock` | Same-machine hosts; 0600, no token |
| mcp-gateway (sandbox) | `:50005` | Reached through the E2B port proxy |

| Component | Env | Purpose |
|---|---|---|
| API Server | `SANDRPOD_TOKEN` | API token |
| API Server | `SANDRPOD_E2B_DOMAIN` | Enables the E2B gateway; the base domain |
| Poder | `API_URL` / `REGION` / `PROVIDER_TYPE` | Server address, region, provider |
| Poder | `SANDRPOD_TOOLBOX_IMAGE` | Sandbox image to run |
| sandrpod-agent | `SANDRPOD_API_URL` / `SANDRPOD_SANDBOX_NAME` / `SANDRPOD_WORK_DIR` | Address, name, workdir |
| sandrpod-agent | `SANDRPOD_PERMISSION_MODE` / `SANDRPOD_AUDIT_*` | Permission gate and audit |

Complete list with defaults: `.env.example`.

---

## 7. Deployment quick reference

```bash
# Local development (memory store)
go run ./cmd/server -port 8080
docker run -d --name sandrpod-poder \
  -v ~/.docker/run/docker.sock:/var/run/docker.sock \
  --add-host host.docker.internal:host-gateway \
  ghcr.io/sandrpod/poder:latest \
  -api-url=http://host.docker.internal:8080 -region=local

# Persistent, single instance
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db

# Agent mode — no Docker, this machine is the sandbox
sandrpod-agent -api-url=http://localhost:8080 -name=my-laptop -work-dir=/tmp/work
sandrpod-cli execute my-laptop "print('hello')" -l python
```

For a real deployment — PostgreSQL, wildcard TLS, the E2B surface on, and the
acceptance sweep that proves it works — follow
[`PRODUCTION_DEPLOYMENT.md`](PRODUCTION_DEPLOYMENT.md).
