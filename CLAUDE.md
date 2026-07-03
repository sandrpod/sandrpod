# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run Commands

```bash
# Build all binaries (dev, native arch)
go build -o server       ./cmd/server
go build -o poder        ./cmd/poder
go build -o agent        ./cmd/agent
go build -o toolbox      ./cmd/toolbox
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
docker buildx build --platform linux/amd64 -f docker/Dockerfile.poder   -t sandrpod/poder:latest   --load .
docker buildx build --platform linux/amd64 -f docker/Dockerfile.toolbox -t sandrpod/toolbox:latest --load .

# Run API Server (port 8080, in-memory store by default)
go run ./cmd/server -port 8080

# Run API Server with SQLite persistence
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db

# Run API Server for cloud providers (AWS/Aliyun/Azure/GCP) — public-url is sent to cloud VMs for callback
go run ./cmd/server -port 8080 -public-url https://api.example.com -db sqlite:./data/sandrpod.db

# Run Poder (requires Docker socket; -network 指定容器网络，默认 bridge)
go run ./cmd/poder -api-url=http://localhost:8080 -region=local
go run ./cmd/poder -api-url=http://localhost:8080 -region=local -network=sandrpod

# Run Poder via Docker (recommended for production)
docker run -d --name sandrpod-poder --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://host.docker.internal:8080 \
  -e SANDRPOD_TOOLBOX_IMAGE=sandrpod/toolbox:latest \
  sandrpod/poder:latest

# Run sandrpod-agent (registers local machine directly as a sandbox)
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-agent

# Run sandrpod-agent in employee-PC mode (permission gate + audit)
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-laptop \
  -permission-mode=prompt \
  -audit-upload-url=https://your-platform/api/audit/decisions/batch

# sandrpod-agent permission/audit flags:
#   -permission-mode   off | prompt | strict  (default: off)
#   -permission-file   override ~/.sandrpod/permissions.json path
#   -audit-dir         local NDJSON log dir (default: ~/.sandrpod/audit; empty=disabled)
#   -audit-upload-url  POST endpoint for audit batch upload (empty=local only)
#   -audit-upload-token bearer token for upload (defaults to -token)
# Env equivalents: SANDRPOD_PERMISSION_MODE, SANDRPOD_PERMISSION_FILE,
#                  SANDRPOD_AUDIT_DIR, SANDRPOD_AUDIT_UPLOAD_URL, SANDRPOD_AUDIT_UPLOAD_TOKEN

# Run sandrpod-tray (user-session GUI companion for employee-PC mode)
sandrpod-tray serve                                      # tray icon + IPC + local settings HTTP
sandrpod-tray rules ls                                   # list permanent rules and hardlocks
sandrpod-tray rules add ~/Documents --mode rw            # add permanent path grant
sandrpod-tray policy ls                                  # show command deny/warn lists
sandrpod-tray unlock ~/.ssh --i-understand-the-risk      # remove a hardlock (CLI only, not GUI)
sandrpod-tray seed                                       # install default hardlock seeds

# Run Toolbox (inside sandbox container, port 8080)
go run ./cmd/toolbox -port 8080
```

## Architecture Overview

SandrPod is an open-source, self-hostable sandbox platform for AI agents — a
drop-in alternative to hosted services like E2B. It provides fast, isolated code
execution environments, exposes **both** a native REST API and the **E2B
wire-protocol** (the unmodified E2B SDK works against it), and schedules
sandboxes across eight cloud providers (incl. Aliyun/Tencent), plain Docker, or a
bare machine via `sandrpod-agent`.

定位：开源、可自托管的 E2B 替代方案 —— 把原封不动的 E2B SDK 指向你自己的多云基础设施
（含阿里云/腾讯云）。同时保留 LangChain/deepagents 原生集成与员工机守护模式。

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
- `pkg/store/`: Repository implementations — in-memory adapter (`memory.go`) and SQLite backend (`sqlite/`). Plug-in via `store.Stores` aggregate.
- `pkg/tunnel/`: WebSocket + yamux reverse tunnel (`PoderTunnel`, `TunnelStore`). Used by both Poder and sandrpod-agent.
- `pkg/toolbox/`: Code execution engine with PTY, file operations, and process management.
- `pkg/permission/`: Decision engine for employee-PC mode. 5-branch policy (work_dir → hardlock → permanent → session → ask). Includes `Store` (atomic JSON), `Manager`, `IPCClient/Server`, command policy scanner, and default hardlock seeds.
- `pkg/notify/`: Cross-platform consent dialog. macOS: `osascript display dialog`; Linux: `zenity`/`kdialog`; Windows: PowerShell `MessageBox`. Fail-close (timeout/error → deny).
- `pkg/audit/`: Local NDJSON audit log (`Recorder`, auto-rotate at 8 MiB) + background HTTP uploader (`Uploader`, at-least-once delivery). Decoupled from `pkg/permission` via `AuditSink` interface.

### State Machine

Sandbox states: `PENDING` → `STARTING` → `RUNNING` → `STOPPING` → `STOPPED` / `ERROR` / `TERMINATED`

### API Endpoints

**Sandboxes**
- `POST /api/v1/sandboxes` - Create sandbox (returns job)
- `GET /api/v1/sandboxes` - List sandboxes
- `GET /api/v1/sandboxes/{name}` - Get sandbox details
- `POST /api/v1/sandboxes/{name}/start` - Start sandbox
- `POST /api/v1/sandboxes/{name}/stop` - Stop sandbox
- `DELETE /api/v1/sandboxes/{name}` - Delete sandbox
- `POST /api/v1/sandboxes/execute` - Execute code (proxied to worker via tunnel)
- `GET /api/v1/sandboxes/stream` - Stream execution output
- `GET /api/v1/sandboxes/{name}/toolbox/*` - Proxy to Toolbox (files upload/download etc.)

**Poder Nodes**
- `GET /api/v1/poders` - List Poder nodes
- `DELETE /api/v1/poders/{id}` - Delete Poder record（若在线则同时断开 tunnel）

**WebSocket / Internal**
- `GET /ws/poder/connect` - Poder registers via WebSocket tunnel (`tunnelStore`)
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

源码：`pkg/sdk/python/cli/`，已安装到本机（开发模式，改源码即时生效）：

```bash
# Sandbox 操作（--provider: local | aws | aliyun | azure | gcp | tencent | digitalocean | hetzner | oracle）
sandrpod-cli list
sandrpod-cli get <name>                                 # 详情
sandrpod-cli env <name>                                 # 运行时环境（arch/OS/shell）
sandrpod-cli create <name> --provider gcp --region asia-east1-a --instance-type e2-medium
sandrpod-cli create <name> --provider local --image sandrpod/toolbox:latest
sandrpod-cli create <name> --poder <poder-id>          # 指定 poder 直建（跳过调度器）
sandrpod-cli create <name> --ttl 3600 --cpu 2 --memory 2048  # 闲置 TTL(秒) + CPU 核 + 内存(MiB)
sandrpod-cli create <name> --no-wait                   # 异步：立即返回 job id，不等 RUNNING
sandrpod-cli start/stop/delete <name>
sandrpod-cli logs <name> [--tail N]
sandrpod-cli execute <name> "ls /workspace"            # 一次性执行
sandrpod-cli stream <name> "for i in 1 2 3; do echo $i; sleep 1; done"  # 流式输出
sandrpod-cli shell <name>                              # 交互式 PTY（需 websocket-client；Ctrl-] 退出）
sandrpod-cli preview <name> <port> [path]              # 访问沙箱内 localhost:<port> 的 web 服务
sandrpod-cli snapshot <name> [--image repo:tag]        # docker commit 成镜像

# Job（异步创建）
sandrpod-cli job get <job-id>                          # 查 async 创建的 job 状态/错误/结果

# 可观测性
sandrpod-cli metrics                                   # 拉取服务端 Prometheus /metrics（需 admin token）

# Poder 管理
sandrpod-cli poder list
sandrpod-cli poder get <poder-id>
sandrpod-cli poder delete <poder-id> [-y] [--keep-vm]  # --keep-vm: 只删记录不终止云 VM

# 文件操作
sandrpod-cli fs ls|cat|write|mkdir|rm|mv|search|grep|info|upload|download <name> ...
sandrpod-cli fs replace <name> <file> <pattern> <new-value>
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
