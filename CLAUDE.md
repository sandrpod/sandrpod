# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run Commands

```bash
# Build all binaries (dev, native arch)
go build -o server  ./cmd/server
go build -o poder   ./cmd/poder
go build -o agent   ./cmd/agent
go build -o toolbox ./cmd/toolbox

# Cross-compile release binaries → dist/
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

# Run API Server for cloud providers (AWS/Aliyun) — public-url is sent to cloud VMs for callback
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

# Run Toolbox (inside sandbox container, port 8080)
go run ./cmd/toolbox -port 8080
```

## Architecture Overview

SandrPod is an AI code execution infrastructure platform providing fast, secure, and scalable sandbox environments.

主要参考daytona的实现，但更简化和轻量化，为AI agent提供代码执行沙箱环境。 实现对langchain deepagents的沙箱环境插件。

未来考虑为openclaw提供标准化的沙箱运行环境。

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

**sandrpod-agent** (`cmd/agent`): Registers the local machine directly as a sandbox via `/ws/sandbox/connect`. Embeds Toolbox — no Docker required. Useful for local development and single-machine setups.

**Toolbox** (`cmd/toolbox`): Code execution service running inside each sandbox container. Provides HTTP API for code execution with PTY support.

### Key Packages

- `pkg/provider/`: Cloud provider abstraction layer (AWS, Aliyun). Factory pattern for dynamic provider registration.
- `pkg/poder/`: Pod executor implementations. Docker implementation for local development.
- `pkg/sandpod/`: SandPod core types, state machine, Repository interfaces (`repo.go`), memory-backed store implementations, Scheduler.
- `pkg/store/`: Repository implementations — in-memory adapter (`memory.go`) and SQLite backend (`sqlite/`). Plug-in via `store.Stores` aggregate.
- `pkg/tunnel/`: WebSocket + yamux reverse tunnel (`PoderTunnel`, `TunnelStore`). Used by both Poder and sandrpod-agent.
- `pkg/toolbox/`: Code execution engine with PTY, file operations, and process management.

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
# Sandbox 操作
sandrpod-cli list
sandrpod-cli create <name> --provider local --image sandrpod/toolbox:latest
sandrpod-cli delete <name>
sandrpod-cli exec <name> "ls /workspace"

# Poder 管理（新）
sandrpod-cli poder list
sandrpod-cli poder delete <poder-id> [-y]
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

- [Daytona](https://github.com/daytonaio/daytona) — sandbox management reference
- [LangChain DeepAgents](https://github.com/langchain-ai/deepagents) — Python SDK integration reference
