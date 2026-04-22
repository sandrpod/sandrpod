---
name: sandrpod-cli
description: SandrPod CLI and SDK for AI sandbox management. Use when working with sandboxes (create/delete/start/stop/execute), Poder worker nodes (poder list/delete), file operations (fs ls/cat/write/grep), persistent sessions, or integrating sandboxes into LangChain/deepagents workflows via langchain-sandrpod. Commands: sandrpod-cli list, create, delete, start, stop, execute, session, fs, poder.
---

# SandrPod CLI Skill

SandrPod is an AI code execution infrastructure platform providing fast, secure, and scalable sandbox environments for AI agents.

## CLI Installation

```bash
# 开发模式（改源码即时生效）
pip install -e pkg/sdk/python/cli

# langchain-sandrpod SDK（deepagents 集成）
pip install -e pkg/sdk/python/langchain_sandrpod
```

## Configuration

```bash
# Set API URL (saves to ~/.sandrpod-cli/config.yaml)
sandrpod-cli set-api-url http://localhost:8080

# Or use environment variable / CLI flag
export SANDRPOD_API_URL=http://localhost:8080
sandrpod-cli --api-url http://localhost:8080 list
```

**Config priority**: CLI flag > Environment > Config file > Default (localhost:8080)

## Sandbox Commands

```bash
# List all sandboxes
sandrpod-cli list

# Create sandbox (指定 toolbox 镜像)
sandrpod-cli create mybox --region local --provider-type local --image sandrpod/toolbox:latest

# Get sandbox info
sandrpod-cli get mybox

# Start / Stop / Delete
sandrpod-cli start mybox
sandrpod-cli stop mybox
sandrpod-cli delete mybox

# Execute code (stateless — each call is a fresh bash process)
sandrpod-cli execute mybox "echo hello"
sandrpod-cli execute mybox "cd /tmp && pwd"  # chain with && to keep state
```

## Poder Commands

```bash
# List Poder worker nodes
sandrpod-cli poder list

# Delete a Poder record (断开 tunnel + 从数据库移除)
sandrpod-cli poder delete <poder-id>
sandrpod-cli poder delete <poder-id> -y   # skip confirmation
```

## Session Commands (Stateful Shell)

Sessions maintain state (cd, environment variables) across commands.

```bash
# Create session
sandrpod-cli session create mybox
sandrpod-cli session create mybox --session-id custom-id

# Execute in session (cd/env persist between calls)
sandrpod-cli session exec mybox <session-id> "cd /tmp"
sandrpod-cli session exec mybox <session-id> "pwd"  # outputs /tmp

# List / Get / Delete sessions
sandrpod-cli session list mybox
sandrpod-cli session get mybox <session-id>
sandrpod-cli session delete mybox <session-id>
```

## File Operations

```bash
# List directory
sandrpod-cli fs ls mybox --path=/workspace

# Read file
sandrpod-cli fs cat mybox /workspace/test.txt

# Write file
sandrpod-cli fs write mybox /workspace/test.txt "hello world"

# Search files (grep)
sandrpod-cli fs grep mybox "TODO" --path=/workspace

# File info
sandrpod-cli fs info mybox /workspace/test.txt
```

## langchain-sandrpod（deepagents / LangChain 集成）

`SandrPodSandbox` 实现 `deepagents.BaseSandbox`，自动获得全套文件操作工具。

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

client = SandrPodClient(api_url="http://localhost:8080")

# 获取已有沙箱
sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "..."}]})

# 上下文管理器（自动创建/删除）
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    result = agent.invoke({"messages": [...]})

# 直接文件 I/O（不经过 Agent）
sb.upload_files([("/workspace/data.csv", csv_bytes)])
sb.download_files(["/workspace/output.txt"])
sb.read("/workspace/output.txt")     # → str
sb.execute("ls /workspace")          # → ExecuteResponse
```

环境变量：`SANDRPOD_API_URL`、`SANDRPOD_API_TOKEN`

完整示例见 `pkg/sdk/python/langchain_sandrpod/examples/`。

## Architecture

```
Client → API Server (Control Plane, :8080)
              ↕ WebSocket + yamux 反向隧道
         Poder (Worker) ──→ Toolbox (Sandbox 容器)

         sandrpod-agent  ──→ (直连模式，本机即沙箱)
```

- **API Server** (port 8080): REST control plane，支持 `-db sqlite:<path>` 持久化
- **Poder**: Worker 节点，管理 Docker 容器生命周期，通过反向 tunnel 接收请求
- **sandrpod-agent**: 本机直连模式，无需 Docker
- **Toolbox**: 沙箱容器内的代码执行服务

## Key API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/sandboxes` | List sandboxes |
| POST | `/api/v1/sandboxes` | Create sandbox |
| DELETE | `/api/v1/sandboxes/{name}` | Delete sandbox |
| POST | `/api/v1/sandboxes/execute` | Execute code |
| GET | `/api/v1/sandboxes/{name}/toolbox/*` | Proxy to Toolbox (files etc.) |
| GET | `/api/v1/poders` | List Poder nodes |
| DELETE | `/api/v1/poders/{id}` | Delete Poder record |

## Project Structure

```
cmd/server/          # API Server
cmd/poder/           # Poder worker node
cmd/agent/           # sandrpod-agent (本机直连)
cmd/toolbox/         # Toolbox 代码执行服务
pkg/sandpod/         # 核心类型、状态机、Repository 接口
pkg/store/           # 存储实现（内存 + SQLite）
pkg/tunnel/          # WebSocket + yamux 反向隧道
pkg/toolbox/         # 代码执行引擎（PTY、文件操作）
pkg/sdk/python/cli/                    # sandrpod-cli
pkg/sdk/python/langchain_sandrpod/     # langchain-sandrpod SDK
  └── examples/      # 示例：质数、销售分析、代码审查
dist/                # 跨平台编译产物
```
