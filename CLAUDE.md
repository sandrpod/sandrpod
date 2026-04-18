# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run Commands

```bash
# Build all binaries
go build -o server ./cmd/server
go build -o poder ./cmd/poder
go build -o toolbox ./cmd/toolbox

# Build Docker images
docker build -f docker/Dockerfile.poder -t sandrpod/poder:test .
docker build -f docker/Dockerfile.toolbox -t sandrpod/toolbox:test .

# Run API Server (port 8080)
go run ./cmd/server -port 8080

# Run Poder/Proxy+Agent (requires Docker socket, no external port needed)
go run ./cmd/poder -api-url=http://localhost:8080 -region=local

# Run Toolbox (inside sandbox container, port 8080)
go run ./cmd/toolbox -port 8080
```

## Architecture Overview

SandrPod is an AI code execution infrastructure platform providing fast, secure, and scalable sandbox environments.

主要参考daytona的实现，但更简化和轻量化，为AI agent提供代码执行沙箱环境。 实现对langchain deepagents的沙箱环境插件。

未来考虑为openclaw提供标准化的沙箱运行环境。

### Core Components

```
Client → API Server (Control Plane) → Proxy+Agent (Worker) → Toolbox (Sandbox)
```

**API Server** (`cmd/server`): REST API control plane. Handles CRUD operations, job management, and acts as central proxy for code execution requests. Does not directly connect to cloud providers.

**Proxy+Agent** (`cmd/poder`): Combined worker node service. Agent polls API Server for CREATE/DELETE sandbox jobs. Worker Proxy forwards code execution requests to Toolbox containers.

**Toolbox** (`cmd/toolbox`): Code execution service running inside each sandbox container. Provides HTTP API for code execution with PTY support.

### Key Packages

- `pkg/provider/`: Cloud provider abstraction layer (AWS, Aliyun). Factory pattern for dynamic provider registration.
- `pkg/poder/`: Pod executor implementations. Docker implementation for local development.
- `pkg/sandpod/`: SandPod core with state machine, job store, sandbox store, and poder store.
- `pkg/toolbox/`: Code execution engine with PTY, file operations, and process management.

### State Machine

Sandbox states: `PENDING` → `STARTING` → `RUNNING` → `STOPPING` → `STOPPED` / `ERROR` / `TERMINATED`

### API Endpoints

- `POST /api/v1/sandboxes` - Create sandbox (returns job)
- `GET /api/v1/sandboxes` - List sandboxes
- `POST /api/v1/sandboxes/execute` - Execute code (proxied to worker)
- `GET /api/v1/sandboxes/stream` - Stream execution output
- `GET /ws/poder/connect` - Poder registers via WebSocket tunnel (replaces HTTP register+heartbeat)
- `GET /api/v1/jobs/poll` - Agent polls for pending jobs

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

- **Python SDK / CLI 源码**：`pkg/sdk/python/`
  - CLI 入口：`pkg/sdk/python/cli/main.py`
  - API 客户端：`pkg/sdk/python/cli/client.py`
  - 已安装到本机：`/opt/miniconda3/bin/sandrpod-cli`（开发模式，修改源码即时生效）

## 参考项目

- daytona的源码：/Users/alice/goworkspace/daytona
- langchain deepagents源码： /Users/alice/pworkspace/deepagents
