# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run Commands

```bash
# Build all binaries (dev, native arch)
go build -o server       ./cmd/server
go build -o poder        ./cmd/poder
go build -o agent        ./cmd/agent
go build -o toolbox      ./cmd/toolbox
go build -o egress-proxy ./cmd/egress-proxy    # per-pod egress proxy (container sandboxes)
# `sandrpod-agent relay` is a hidden subcommand: the in-sandbox bridge used when
# the OS sandbox takes the network away (Linux). Not a separate binary.
go build -o sandrpod-tray ./cmd/sandrpod-tray  # CGO required (systray uses Cocoa/GTK/win32)

# One-shot cross-compile: all platforms + sandrpod-tray (auto-skips missing toolchains)
make build-all
# Outputs: dist/server-linux-amd64, dist/sandrpod-agent-{linux,darwin,windows}-{amd64,arm64}[.exe]
#          dist/sandrpod-tray-darwin-{amd64,arm64}, dist/sandrpod-tray-windows-amd64.exe (needs mingw-w64)
#          dist/sandrpod-tray-linux-{amd64,arm64} (needs docker)

# Cross-compile release binaries → dist/ (agent/server are CGO=0; tray needs CGO)
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/server-linux-amd64          ./cmd/server
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-linux-amd64  ./cmd/agent
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags="-s -w" -o dist/sandrpod-agent-linux-arm64  ./cmd/agent
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-darwin-amd64 ./cmd/agent
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o dist/sandrpod-agent-darwin-arm64 ./cmd/agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-windows-amd64.exe ./cmd/agent

# Build Docker images (amd64)
docker buildx build --platform linux/amd64 -f docker/Dockerfile.poder   -t ghcr.io/sandrpod/poder:latest   --load .
docker buildx build --platform linux/amd64 -f docker/Dockerfile.toolbox -t ghcr.io/sandrpod/toolbox:latest --load .
docker buildx build --platform linux/amd64 -f docker/Dockerfile.egress  -t ghcr.io/sandrpod/egress-proxy:latest --load .   # or: make docker-egress

# Run API Server (port 8080, in-memory store by default)
go run ./cmd/server -port 8080

# Run API Server with SQLite persistence (dev / single instance)
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db

# Run API Server with PostgreSQL (production: connection pool + concurrent job claim via SELECT FOR UPDATE SKIP LOCKED)
go run ./cmd/server -port 8080 -db "postgres://user:pass@host:5432/sandrpod?sslmode=require"
# One instance uses ONE backend, selected at startup by the -db DSN scheme; the same pkg/store/sqldb code targets either SQLite or PostgreSQL (not both at once).

# Multi-instance LOAD mode (N active instances behind a load balancer, shared PostgreSQL):
#   give each instance a unique -node-url (its internal address peers reach it at).
#   A request landing on any instance is forwarded to the node holding the target poder's tunnel.
go run ./cmd/server -port 8080 -db "postgres://…" -node-url http://10.0.1.5:8080   # instance A
go run ./cmd/server -port 8080 -db "postgres://…" -node-url http://10.0.1.6:8080   # instance B (same DB)

# Run API Server for cloud providers (AWS/Aliyun/Azure/GCP) — public-url is sent to cloud VMs for callback
go run ./cmd/server -port 8080 -public-url https://api.example.com -db sqlite:./data/sandrpod.db

# Run Poder (requires Docker socket; -network 指定容器网络，默认 bridge)
#   On macOS run it in Docker instead (see the next block): a poder on the host
#   cannot reach a container's bridge IP (Docker Desktop runs the daemon in a VM),
#   so `execute` hangs for the full timeout and then answers 500/503. On Linux
#   both work. This costs an hour if you hit it without knowing.
go run ./cmd/poder -api-url=http://localhost:8080 -region=local
go run ./cmd/poder -api-url=http://localhost:8080 -region=local -network=sandrpod
#   -emulate-legacy (poder and server, TEST ONLY) makes the binary behave like a
#   build from before the capability bits — it advertises nothing — so the
#   nightly can prove the mixed-version refusals without an old binary.

# Run Poder via Docker (recommended for production)
docker run -d --name sandrpod-poder --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://host.docker.internal:8080 \
  -e SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:latest \
  ghcr.io/sandrpod/poder:latest

# Run sandrpod-agent (registers local machine directly as a sandbox)
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-agent

# Run sandrpod-agent in employee-PC mode (permission gate + audit)
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-laptop \
  -permission-mode=prompt \
  -egress-mode=enforce -egress-policy=~/.sandrpod/egress.json \
  -audit-upload-url=https://your-platform/api/audit/decisions/batch

# sandrpod-agent permission/audit/egress flags:
#   -permission-mode   off | prompt | strict  (default: off)
#   -permission-file   override ~/.sandrpod/permissions.json path
#   -audit-dir         local NDJSON log dir (default: ~/.sandrpod/audit; empty=disabled)
#   -audit-upload-url  POST endpoint for audit batch upload (empty=local only)
#   -audit-upload-token bearer token for upload (defaults to -token)
#   -egress-mode       off | enforce (local file) | server (polled copy) | decider (per connection)  (default: off)
#   -egress-policy     path to the egress allow-list JSON (required with enforce)
#   -egress-sync       how often server mode re-reads the policy (default 60s)
#   -os-sandbox        off | auto | bwrap | seatbelt  (default: off)
#   -credential-mask        comma-separated credential names to mask (values from the agent's env)
#   -credential-mask-files  credential files rewritten with sentinels and bound over the originals
#   -credential-ca-dir      egress CA used to terminate TLS (default ~/.sandrpod/egress-ca)
#   -credential-terminate-exclude  domains never terminated (mTLS, certificate pinning)
#   -credential-allow-plaintext    allow a masked credential over HTTP (not recommended)
#   -mandatory-deny-depth  how deep below work_dir to write-protect persistence
#                      paths (git hooks/config, .bashrc, .mcp.json, IDE config; default 3)
# Env equivalents: SANDRPOD_PERMISSION_MODE, SANDRPOD_PERMISSION_FILE,
#                  SANDRPOD_AUDIT_DIR, SANDRPOD_AUDIT_UPLOAD_URL, SANDRPOD_AUDIT_UPLOAD_TOKEN,
#                  SANDRPOD_EGRESS_MODE, SANDRPOD_EGRESS_POLICY, SANDRPOD_EGRESS_SYNC,
#                  SANDRPOD_OS_SANDBOX, SANDRPOD_MANDATORY_DENY_DEPTH,
#                  SANDRPOD_CREDENTIAL_MASK, SANDRPOD_CREDENTIAL_MASK_FILES,
#                  SANDRPOD_CREDENTIAL_CA_DIR, SANDRPOD_CREDENTIAL_TERMINATE_EXCLUDE,
#                  SANDRPOD_CREDENTIAL_ALLOW_PLAINTEXT

# Run sandrpod-tray (user-session GUI companion for employee-PC mode)
sandrpod-tray serve                                      # tray icon + IPC + local settings HTTP
sandrpod-tray rules ls                                   # list permanent rules and hardlocks
sandrpod-tray rules add ~/Documents --mode rw            # add permanent path grant
sandrpod-tray policy ls                                  # show command deny/warn lists
sandrpod-tray unlock ~/.ssh --i-understand-the-risk      # remove a hardlock (CLI only, not GUI)
sandrpod-tray seed                                       # install default hardlock seeds

# Run Toolbox (inside sandbox container, port 8080)
go run ./cmd/toolbox -port 8080

# Playground upstream for manual verification (see docs/PLAYGROUND.md)
go run ./hack/playground/echo -gen-ca /tmp/pg/certs     # self-signed CA + cert for echo.local
go run ./hack/playground/echo -addr :8443 -cert /tmp/pg/certs/cert.pem -key /tmp/pg/certs/key.pem
```

## Architecture Overview

SandrPod is open-source, self-hosted **execution infrastructure (a control
plane) for AI agents**. It turns your own substrate into on-demand sandboxes and
rests on three pillars: **run anywhere you own** (8 clouds incl. Aliyun/Tencent,
plain Docker, or a bare machine with no Docker via `sandrpod-agent`); **speak any
SDK** (native REST/Python/TS, LangChain/deepagents, and the unmodified E2B SDK as
a drop-in); **stay in control** (reverse-tunnel workers with zero inbound ports,
opt-in permission gate + decision audit, self-hosted data).

Docs voice: lead with the three pillars above. Treat E2B compatibility as one
supporting feature of "speak any SDK", not as the product's identity.

### Core Components

```
Client → API Server (Control Plane)
              ↕ WebSocket + yamux 反向隧道
         Poder (Worker)  ──→ Toolbox (Sandbox 容器)

         sandrpod-agent  ──→ (直连模式，本机即沙箱)
              ↑ 直接 WebSocket 注册到 API Server
```

**API Server** (`cmd/server`): REST API control plane. Handles CRUD operations, job management, and acts as central proxy for code execution requests. Routes requests via `tunnelStore` (Poder nodes) or `directStore` (Agent nodes) depending on `proxy_url` prefix.

**Proxy+Agent** (`cmd/poder`): Combined worker node service. Dials API Server via WebSocket reverse tunnel (`/ws/poder/connect`). Polls for CREATE/DELETE sandbox jobs. Manages Docker container lifecycle.

**sandrpod-agent** (`cmd/agent`): Registers the local machine directly as a sandbox via `/ws/sandbox/connect`. Embeds Toolbox — no Docker required. Supports opt-in permission gate (`--permission-mode`) and audit pipeline for employee-PC deployments.

**sandrpod-tray** (`cmd/sandrpod-tray`): User-session GUI daemon for employee-PC mode. Provides tray icon, native consent dialogs (osascript/zenity/PowerShell), and a local HTTP settings page. Communicates with `sandrpod-agent` over `~/.sandrpod/authz.sock`. See `docs/PERMISSION_AND_AUDIT.md`.

**Toolbox** (`cmd/toolbox`): Code execution service running inside each sandbox container. Provides HTTP API for code execution with PTY support.

### Key Packages

- `pkg/provider/`: Cloud provider abstraction layer (AWS, Aliyun, Azure, GCP, Tencent, DigitalOcean, Hetzner, Oracle). Factory pattern for dynamic provider registration. Two remote-exec backends: **managed run-command** (AWS SSM / Aliyun CloudAssist / Azure Run Command / Tencent TAT / Oracle Instance Agent) and **SSH** for clouds with no such API (GCP, DigitalOcean, Hetzner). The SSH path uses a per-VM ephemeral ed25519 key — GCP injects it via instance metadata + sudo (`pkg/provider/gcp/ssh.go`), while DigitalOcean/Hetzner inject it via cloud-init as root and share `pkg/provider/sshexec`.
- `pkg/poder/`: Pod executor implementations. Docker implementation for local development.
- `pkg/sandpod/`: SandPod core types, state machine, Repository interfaces (`repo.go`), memory-backed store implementations, Scheduler.
- `pkg/store/`: Repository implementations — in-memory adapter (`memory.go`) and a dialect-parameterised SQL backend (`sqldb/`) that targets **either SQLite or PostgreSQL from one codebase** (a thin `Dialect` handles placeholder rebinding, DDL types, and the `FOR UPDATE SKIP LOCKED` job claim). A given server instance uses exactly one backend, chosen at startup by the `-db` DSN scheme (`sqlite:` / `postgres://`) — not both simultaneously. Plug-in via `store.Stores` aggregate.
- `pkg/tunnel/`: WebSocket + yamux reverse tunnel (`PoderTunnel`, `TunnelStore`). Used by both Poder and sandrpod-agent.
- `pkg/toolbox/`: Code execution engine with PTY, file operations, and process management.
- `pkg/permission/`: Decision engine for employee-PC mode. 6-branch policy (hardlock → mandatory deny → work_dir → permanent → session → ask). `mandatory.go` holds the always-write-denied set inside a work tree (git hooks/config, shell startup files, `.mcp.json`, IDE launch config) plus `MandatoryDenyPaths` for the OS-sandbox walk. Includes `Store` (atomic JSON), `Manager`, `IPCClient/Server`, command policy scanner, and default hardlock seeds.
- `pkg/notify/`: Cross-platform consent dialog. macOS: `osascript display dialog`; Linux: `zenity`/`kdialog`; Windows: PowerShell `MessageBox`. Fail-close (timeout/error → deny).
- `pkg/audit/`: Local NDJSON audit log (`Recorder`, auto-rotate at 8 MiB) + background HTTP uploader (`Uploader`, at-least-once delivery). Decoupled from `pkg/permission` via `AuditSink` interface.
- `pkg/sandbox/`: OS sandbox for spawned processes (see `docs/OS_SANDBOX.md`). `Bwrap` (Linux) renders a bubblewrap mount plan — read-only root, writable `work_dir` + temp dirs, `--ro-bind /dev/null`/`--tmpfs` over hardlocks, and, when egress is enforcing, `--unshare-net` plus read-only binds of the agent binary (as `sandrpod-agent relay`) and the proxy's unix socket, with a shell wrapper that starts the relay, waits for its ready file and `exec "$@"`; `Seatbelt` (macOS) renders an SBPL profile with the same shape. Installed by `cmd/agent -os-sandbox` from the permission store's hardlocks and mandatory-deny walk (`Policy.WriteDeny` → `--ro-bind` islands / trailing SBPL `deny file-write*`); on macOS, egress enforcing also sets `Policy.ProxyOnlyPort`, narrowing outbound to the proxy's loopback port because there is no namespace to remove, applied at every toolbox spawn site through `toolbox.SetSandbox`/`wrapSandbox` (`pkg/toolbox/sandbox.go`). `auto` degrades loudly, `bwrap`/`seatbelt` refuse startup when the backend's probe fails.
- `pkg/egress/credential.go`, `mitm.go`, `terminate.go`, `maskfiles.go`: credential masking (see `docs/CREDENTIALS.md`). `Masker` holds sentinel→real pairs in memory only (`fake_value_<uuid4>`, padded to the real length so `Content-Length` survives), substitutes in headers and bodies, and re-signs AWS SigV4 requests (`ResignAWS`, verified against AWS's published signature). `CA` (ECDSA P-256, persisted, key `0600`) signs a per-host leaf; `terminate.go` hijacks an allowed CONNECT, completes TLS with that leaf (ALPN `http/1.1` only, so h2 cannot skip substitution), and re-originates the request through the guarded dialer. Masking *requires* termination (`Start` refuses otherwise), cleartext requests carrying a sentinel are refused (`masked_credential_cleartext`) unless `-credential-allow-plaintext`, and `TerminateOptions.Exclude` keeps mTLS/pinning hosts opaque at the cost of masking them. `MaskFile` rewrites credentials files for `Policy.ReadSubstitute` (bwrap `--ro-bind fake real`; macOS degrades to a read deny).
- `pkg/relay/`: The in-sandbox bridge (TCP on loopback → the proxy's unix socket), run as `sandrpod-agent relay --listen 127.0.0.1:3128 --unix <sock> --ready <file>`. It carries no policy: every byte goes to the proxy, which decides.
- `cmd/egress-proxy/`: The egress proxy as a standalone process, run as a per-pod sidecar container for container sandboxes (`docker/Dockerfile.egress`, `make docker-egress`, image overridable with `SANDRPOD_EGRESS_IMAGE`).
- `pkg/egress/`: Egress policy for agent mode (see `docs/EGRESS_POLICY.md`). `Policy`/`Compiled` match `host[:port]` entries (`*.` wildcards, IPv6 literals); `guard.go` resolves an allowed name once and refuses loopback / link-local / cloud metadata / this host's own addresses before dialing the vetted address; `proxy.go` + `socks.go` serve an HTTP (absolute-form + CONNECT) and SOCKS5 (CONNECT only) proxy bound to `127.0.0.1` behind a per-process token. `Options.Decide` + `DecideCacheTTL` turn it into a decider-mode proxy (verdict per connection, fail-closed, 10s cache). Denials carry a model-facing reason (`X-Deny-Reason`) and are audited as `source: "egress"`. Wired in by `cmd/agent`: `-egress-mode=enforce` reads a local file, `-egress-mode=server` starts deny-all and polls `GET /api/v1/sandboxes/{name}/egress`, hot-swapping through `Server.UpdatePolicy`. The proxy env goes to every toolbox-spawned process through `toolbox.SetChildEnv`/`ChildEnv` (`pkg/toolbox/env.go`), never into the agent's own traffic. The policy is stored with the sandbox record (`SandboxInfo.Network`, `sandboxes.egress_policy`), set at create time, and editable via `PUT`. Container pods (poder) enforce it differently: `pkg/poder/egress.go` creates a per-pod `Internal: true` docker network, runs the `cmd/egress-proxy` sidecar on it plus an ordinary network, and attaches the sandbox to the internal one only — so the container has no route except the proxy. If the proxy cannot be started, `CreatePod` fails rather than silently running unenforced.

### State Machine

Sandbox states: `PENDING` → `STARTING` → `RUNNING` → `STOPPING` → `STOPPED` / `ERROR` / `TERMINATED`

### API Endpoints

**Sandboxes**
- `POST /api/v1/sandboxes` - Create sandbox (returns job). A `network` policy on a fleet with no capable worker answers **409** ("worker capability unavailable") instead of creating an unenforced sandbox
- `GET /api/v1/sandboxes` - List sandboxes
- `GET /api/v1/sandboxes/{name}` - Get sandbox details
- `POST /api/v1/sandboxes/{name}/start` - Start sandbox
- `POST /api/v1/sandboxes/{name}/stop` - Stop sandbox
- `DELETE /api/v1/sandboxes/{name}` - Delete sandbox
- `POST /api/v1/sandboxes/execute` - Execute code (proxied to worker via tunnel)
- `GET /api/v1/sandboxes/stream` - Stream execution output
- `GET /api/v1/sandboxes/{name}/toolbox/*` - Proxy to Toolbox (files upload/download etc.)
- `GET /api/v1/capabilities` - What this control plane can do (`egress_policy`, `egress_decider`, ...). The agent probes it before starting `-egress-mode=server|decider` and exits with a clear message when the server predates the endpoints, instead of running deny-all and looking like an outage
- `GET /api/v1/sandboxes/{name}/egress` - Egress policy of a sandbox (`{"sandbox":…,"policy":{…}|null}`); the agent in `-egress-mode=server` polls this
- `POST /api/v1/policy/decide` - Per-connection verdict (`{"sandbox","host","port"}` → `{"allow","code","rule","reason"}`); `-egress-mode=decider` calls it for every destination. Answers 200 with a denial in-band: an HTTP error would surface as "decider unavailable", which is a different fact
- `PUT /api/v1/sandboxes/{name}/egress` - Replace the policy (validated; applies to the sandbox's next connection)
- `DELETE /api/v1/sandboxes/{name}/egress` - Remove the policy (back to "unconfigured"; not the same as `PUT {}`, which allows nothing)

**Poder Nodes**
- `GET /api/v1/poders` - List Poder nodes
- `DELETE /api/v1/poders/{id}` - Delete Poder record（若在线则同时断开 tunnel）

**WebSocket / Internal**
- `GET /ws/poder/connect` - Poder registers via WebSocket tunnel (`tunnelStore`); headers carry its identity, resources and **capabilities** (`X-Poder-Capabilities: egress`). A sandbox that asks for a policy is only scheduled onto a worker that advertises the capability — the alternative is a sandbox reported as walled that runs unwalled.
- `GET /ws/sandbox/connect` - sandrpod-agent registers local machine as sandbox (`directStore`)
- `GET /api/v1/jobs/poll` - Poder polls for pending CREATE/DELETE jobs

### Network Architecture

| Service | Container Port | Host Port |
|---------|--------------|-----------|
| API Server | 8080 | 8080 |
| Proxy+Agent | — | — (no external port, dials API Server via WebSocket tunnel) |
| Toolbox | 8080 | 18080 (test only) |

### Provider Interface

`pkg/provider/interface.go` defines the Provider interface for multi-cloud support. Implementations must provide: `CreateVM`, `DeleteVM`, `GetVM`, `ListVMs`, `ExecuteCommand`, `WaitUntilRunning`, `GetHealthStatus`.

### Poder Interface

`pkg/poder/interface.go` defines the Poder interface for pod execution. Implementations (e.g., Docker in `pkg/poder/docker.go`) must provide: `CreatePod`, `DeletePod`, `PausePod`, `UnpausePod`, `GetPodLogs`, `GetToolboxInfo`.

## SDK & CLI

### sandrpod-cli（Python）

源码：`pkg/sdk/python/sandrpod_cli/`（开发时可 `pip install -e pkg/sdk/python`，改源码即时生效）：

```bash
# Sandbox 操作（--provider: local | aws | aliyun | azure | gcp | tencent | digitalocean | hetzner | oracle）
sandrpod-cli list
sandrpod-cli get <name>                                 # 详情
sandrpod-cli env <name>                                 # 运行时环境（arch/OS/shell）
sandrpod-cli create <name> --provider gcp --region asia-east1-a --instance-type e2-medium
sandrpod-cli create <name> --provider local --image ghcr.io/sandrpod/toolbox:latest
sandrpod-cli create <name> --poder <poder-id>          # 指定 poder 直建（跳过调度器）
sandrpod-cli create <name> --ttl 3600 --cpu 2 --memory 2048  # 闲置 TTL(秒) + CPU 核 + 内存(MiB)
sandrpod-cli create <name> --egress-policy policy.json      # 建时带出网白名单（同 PUT /egress 的 JSON）
sandrpod-cli create <name> --no-wait                   # 异步：立即返回 job id，不等 RUNNING
sandrpod-cli start/stop/delete <name>
sandrpod-cli logs <name> [--tail N]
sandrpod-cli execute <name> "ls /workspace"            # 一次性执行（无状态）
sandrpod-cli stream <name> "for i in 1 2 3; do echo $i; sleep 1; done"  # 流式输出（实时）
sandrpod-cli shell <name>                              # 交互式 PTY（需 websocket-client；Ctrl-] 退出）
sandrpod-cli preview <name> <port> [path]              # 访问沙箱内 localhost:<port> 的 web 服务
sandrpod-cli snapshot <name> [--image repo:tag]        # docker commit 成镜像

# 有状态代码解释器（Jupyter 式，变量跨调用保留）
sandrpod-cli run <name> "z = 10" --context <ctx-id>    # 在有状态内核里执行；同 context 内 z 保留
sandrpod-cli context create <name>                     # 新建上下文（独立命名空间），打印 id
sandrpod-cli context list <name>                       # 列出上下文
sandrpod-cli context restart <name> <ctx-id>           # 重启内核（清空命名空间，保留 id）
sandrpod-cli context rm <name> <ctx-id>                # 销毁上下文

# 目录监视 + 每沙箱资源
sandrpod-cli fs watch <name> <path> [--recursive]      # 监视目录，实时打印 create/write/remove 等事件
sandrpod-cli stats <name>                              # 单沙箱 CPU/内存/磁盘用量（区别于下面服务端 metrics）

# Job（异步创建）
sandrpod-cli job get <job-id>                          # 查 async 创建的 job 状态/错误/结果

# 可观测性
sandrpod-cli metrics                                   # 拉取服务端 Prometheus /metrics（需 admin token）

# API token 签发（admin；签发的 key 是 e2b_<hex>，可直接当 E2B_API_KEY；只存 hash、裸 key 只显一次）
sandrpod-cli token create <name> [--role user|admin]   # 签发，打印裸 key（仅此一次）
sandrpod-cli token list                                # 列出（只有 prefix/name/role/created）
sandrpod-cli token rm <prefix>                         # 按前缀吊销（需 -db 持久化；详见 docs/AUTH_AND_KEYS.md）

# Poder 管理
sandrpod-cli poder list
sandrpod-cli poder get <poder-id>
sandrpod-cli poder delete <poder-id> [-y] [--keep-vm]  # --keep-vm: 只删记录不终止云 VM

# 文件操作
sandrpod-cli fs ls|cat|write|mkdir|rm|mv|search|grep|info|upload|download <name> ...
sandrpod-cli fs replace <name> <file> <pattern> <new-value>

# 出网策略（egress）
sandrpod-cli egress get <name>                              # 打印策略 JSON；"null" = 未配置
sandrpod-cli egress set <name> policy.json                  # 支持文件 / - (stdin) / 内联 JSON
sandrpod-cli create <name> --allow-domain a.example --deny-domain b.example   # 快捷写法（可重复）
sandrpod-cli egress clear <name> [-y]                       # 移除策略（≠ set {}，空对象是“拒绝一切”）
```

### langchain-sandrpod（Python SDK for deepagents）

源码：`pkg/sdk/python/langchain_sandrpod/`

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent

client = SandrPodClient(api_url="http://localhost:8080")
sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)

# 或用上下文管理器自动创建/删除
with client.sandbox("temp-sb") as sb:
    result = agent.invoke({"messages": [...]})
```

环境变量：`SANDRPOD_API_URL`、`SANDRPOD_API_TOKEN`。示例见 `pkg/sdk/python/langchain_sandrpod/examples/`。

## 参考项目

- [LangChain DeepAgents](https://github.com/langchain-ai/deepagents) — Python SDK integration reference
