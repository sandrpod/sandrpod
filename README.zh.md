<p align="center">
  <img src="assets/logo.png" alt="SandrPod" width="400"/>
</p>

<p align="center">
  <strong>为 AI Agent 构建的轻量级沙箱执行基础设施</strong>
</p>

<p align="center">
  <a href="https://pypi.org/project/langchain-sandrpod/"><img src="https://img.shields.io/pypi/v/langchain-sandrpod?color=3B82F6&label=langchain-sandrpod" alt="PyPI"/></a>
  <img src="https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go" alt="Go"/>
  <img src="https://img.shields.io/badge/license-Apache%202.0-green" alt="License"/>
</p>

---

## 简介

**SandrPod** 是一个轻量级开源沙箱基础设施平台，专为 AI Agent 设计。它提供快速、隔离的代码执行环境，Agent 可以按需创建沙箱、在其中执行代码，并在完成后销毁。

API Server 通过 WebSocket 反向隧道连接工作节点（Poder），每个沙箱内运行一个 Toolbox 服务，负责处理 Shell 执行、文件 I/O 和持久会话。工作节点无需暴露任何外部端口，所有流量均通过隧道转发。

### 核心特性

- **即开即用的沙箱** — 通过 Docker 在秒级内启动隔离的执行环境
- **Agent 原生 API** — 专为程序化控制设计的简洁 REST 接口
- **LangChain 集成** — `langchain-sandrpod` 将沙箱直接接入 deepagents，为任意 LLM Agent 提供完整的文件系统和 Shell 能力
- **持久会话** — 在多次调用之间保持 Shell 状态（工作目录、环境变量）
- **SQLite 持久化** — 通过 `-db` 参数一键启用持久存储，默认内存模式
- **多节点调度** — 跨区域连接多个 Poder 工作节点，调度器自动选择负载最低的节点
- **直连 Agent 模式** — 无需 Docker，通过 `sandrpod-agent` 将任意机器注册为沙箱
- **反向隧道架构** — Poder 主动拨入 API Server，工作节点无需暴露外部端口
- **权限网关（可选）** — 员工 PC 模式下的路径同意弹窗、命令黑名单、PTY 同意；借鉴 macOS TCC 模型，通过 `sandrpod-tray` 跨平台支持
- **决策审计管道（可选）** — 每条 allow/deny/warn 决策本地落 NDJSON 并推送至中央 HTTP 端点，满足合规和可观测性需求

---

## 架构

```
Client → API Server (Control Plane, :8080)
              ↕ WebSocket + yamux 反向隧道
         Poder (Worker) ──→ Toolbox (Sandbox 容器)

         sandrpod-agent  ──→ (直连模式，本机即沙箱)
```

| 组件 | 说明 |
|------|------|
| **API Server** | REST 控制面，处理沙箱 CRUD、任务调度，通过隧道代理请求 |
| **Poder** | 工作节点，主动建立 WebSocket 长连接，管理 Docker 容器生命周期 |
| **sandrpod-agent** | 将本机直接注册为沙箱，内嵌 Toolbox，无需 Docker；支持 `--permission-mode` 权限网关和审计管道 |
| **sandrpod-tray** | 员工 PC 模式的用户会话 GUI 守护进程：托盘图标、原生同意弹窗、本地设置页。详见 [docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md) |
| **Toolbox** | 运行在沙箱容器内的代码执行服务，支持 PTY、文件操作 |

> **员工 PC 模式（可选）**：当 `sandrpod-agent` 运行在真实员工笔记本而非服务器上时，可开启路径同意 + 命令黑名单 + PTY 同意的权限网关，以及将所有 allow/deny/warn 决策推送至中央 HTTP 端点的审计管道。两层均默认关闭（`--permission-mode=off`），完整说明见 **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**。

---

## 快速开始

### 1. 启动 API Server

```bash
# 内存模式（默认）
go run ./cmd/server -port 8080

# SQLite 持久化模式（推荐）
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db
```

### 2. 启动 Poder（Docker 方式）

```bash
docker run -d --name sandrpod-poder --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://host.docker.internal:8080 \
  -e SANDRPOD_TOOLBOX_IMAGE=sandrpod/toolbox:latest \
  sandrpod/poder:latest
```

> Poder 不需要暴露任何外部端口，它主动向 API Server 建立 WebSocket 反向隧道。

### 3. 单机直连模式（无需 Docker）

```bash
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-machine
```

### 4. 员工 PC 模式（权限网关 + 审计）

```bash
# 启动托盘伴侣（用户会话，菜单栏出现 🛡 图标）
sandrpod-tray serve

# 启动 agent，开启权限网关和审计上报
go run ./cmd/agent \
  -api-url=http://localhost:8080 \
  -name=my-laptop \
  -permission-mode=prompt \
  -audit-upload-url=https://your-platform/api/audit/decisions/batch
```

权限模式：`off`（默认，仅系统路径黑名单）| `prompt`（work_dir 外路径弹同意框）| `strict`（work_dir 外静默拒绝，适合无头服务器）。

详见 **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**。

### 5. 执行代码

```bash
# 通过 REST API
curl -X POST "http://localhost:8080/api/v1/sandboxes/execute?sandbox=my-sandbox" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","code":"print(\"hello world\")"}'
```

---

## LangChain / deepagents 集成

```bash
pip install langchain-sandrpod
```

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

model = ChatOpenAI(model="gpt-4o", temperature=0)
client = SandrPodClient(api_url="http://localhost:8080")

# 获取已有沙箱
sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "写一个快速排序并运行"}]})

# 或用上下文管理器自动创建/删除
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    result = agent.invoke({"messages": [...]})
```

更多示例见 [`pkg/sdk/python/langchain_sandrpod/examples/`](pkg/sdk/python/langchain_sandrpod/examples/)。

---

## CLI

```bash
pip install sandrpod-cli

sandrpod-cli set-api-url http://localhost:8080

sandrpod-cli list
sandrpod-cli create my-sandbox --provider-type local --image sandrpod/toolbox:latest
sandrpod-cli execute my-sandbox "ls /workspace"
sandrpod-cli delete my-sandbox

# Poder 管理
sandrpod-cli poder list
sandrpod-cli poder delete <poder-id>
```

---

## API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/sandboxes` | 列出沙箱 |
| POST | `/api/v1/sandboxes` | 创建沙箱 |
| DELETE | `/api/v1/sandboxes/{name}` | 删除沙箱 |
| POST | `/api/v1/sandboxes/execute` | 执行代码 |
| GET | `/api/v1/sandboxes/{name}/toolbox/*` | 代理到 Toolbox（文件上传/下载等） |
| GET | `/api/v1/poders` | 列出 Poder 节点 |
| DELETE | `/api/v1/poders/{id}` | 删除 Poder 记录 |

---

## 构建

```bash
# 一键构建：全平台 + sandrpod-tray（缺少工具链时自动跳过）
make build-all

# 本地构建（agent/server 无需 CGO；tray 需要 CGO + 原生库）
go build -o server        ./cmd/server
go build -o poder         ./cmd/poder
go build -o agent         ./cmd/agent
go build -o sandrpod-tray ./cmd/sandrpod-tray   # 需要 CGO

# 跨平台编译（仅 agent + server，CGO=0）
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/server-linux-amd64 ./cmd/server
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-linux-amd64 ./cmd/agent
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o dist/sandrpod-agent-darwin-arm64 ./cmd/agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-windows-amd64.exe ./cmd/agent

# sandrpod-tray（CGO）：macOS 直接 host build；Linux 通过 Docker；Windows 需要 mingw-w64
# `make build-all` 自动处理三个平台。

# Docker 镜像（amd64）
docker buildx build --platform linux/amd64 -f docker/Dockerfile.poder   -t sandrpod/poder:latest   --load .
docker buildx build --platform linux/amd64 -f docker/Dockerfile.toolbox -t sandrpod/toolbox:latest --load .
```

---

## License

Apache 2.0
