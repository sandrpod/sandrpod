# SandrPod 架构文档

> **版本**: v0.3  
> **更新日期**: 2026-04-19  
> 本文档描述当前**已实现并正常运行**的架构。历史设计规划见 [`design/architecture-v1.md`](design/architecture-v1.md)，横向扩展方案见 [`design/horizontal-scaling.md`](design/horizontal-scaling.md)。

---

## 一、系统概述

SandrPod 是面向 AI Agent 的代码执行沙箱平台。核心理念：

- **API Server** 是唯一的控制平面 + 请求代理，客户端只与它通信
- **Poder**（Worker 节点）主动拨出 WebSocket 反向隧道，API Server 通过隧道下发请求，无需 Worker 暴露任何端口
- **sandrpod-agent** 让任意本地机器直接注册为沙箱，绕过 Poder/Docker 层（toC 场景）
- **Toolbox** 运行在每个沙箱容器内，提供代码执行 HTTP API

```
Client / SDK / CLI
        │  HTTP
        ▼
┌──────────────��────────────────────────────────────┐
│              API Server  :8080                     │
│  ┌─────────────────┐   ┌────────────────���─────┐   │
│  │  Control Plane  │   │  Tunnel Proxy        │   │
│  │  - Sandbox CRUD │   │  - 反代到 Poder 隧道  │   │
│  │  - Job 管理     │   │  - 反代到 Agent 隧道  │   │
│  │  - Poder 注册   │   │  - 路由 execute/stream│   │
│  └─────────────────���   └────────────────��─────┘   │
│                                                     │
│  ┌──────────────���──┐   ┌──────────────────────┐   │
│  │  tunnelStore    │   │  directStore         │   │
│  │  poderID→tunnel │   │  sandboxName→tunnel  │   │
│  └─────────────────┘   └──────────────────────┘   │
│                                                     │
│  ┌─────────────────────────────────────────────┐   │
│  │  Store (SQLite / 内存)                       │   │
│  │  SandboxRepository  PoderRepository          │   │
│  │  JobRepository                               │   │
│  └─────────────────────────────────────────────┘   │
└──────────────┬──────────────────┬────────────────��┘
               │ WebSocket 反向隧道│ WebSocket 反向隧道
               ▼                  ▼
   ┌────────────────┐    ┌────────────────────��─┐
   │   Poder        │    │   sandrpod-agent     │
   │ (Docker Worker)│    │  (本机直连沙箱)       │
   │                │    │  嵌入 Toolbox         │
   │ ┌────────────┐ │    └──────────────────────┘
   │ │  Toolbox   │ │
   │ │ (容器内)    │ │
   │ └────────────┘ │
   └────────────────┘
```

---

## 二、组件详解

### 2.1 API Server（`cmd/server`）

**职责**：
- 控制平面：Sandbox / Poder / Job 的 CRUD
- 隧道管理：接收 Poder 和 Agent 的 WebSocket 连接，维护 yamux 多路复用隧道
- 请求代理：所有代码执行、文件操作、PTY 等请求通过隧道转发

**启动参数**：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-port` | `8080` | 监听端口 |
| `-token` | `""` | API 认证 token（空=不鉴权） |
| `-db` | `""` | 存储后端：空=内存；`sqlite:<path>`=SQLite |
| `-offline-timeout` | `30s` | Poder 心跳超时后标记 OFFLINE |

**WebSocket 端点**：

| 端点 | 用途 |
|------|------|
| `GET /ws/poder/connect` | Poder 拨入，注册并建立 yamux 反向隧道 |
| `GET /ws/sandbox/connect` | sandrpod-agent 拨入，直接注册为 Sandbox |

**HTTP API**：

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/health` | 健康检查 |
| `GET` | `/api/v1/poders` | 列出所有 Poder |
| `POST` | `/api/v1/sandboxes` | 创建沙箱（下发 Job） |
| `GET` | `/api/v1/sandboxes` | 列出所有沙箱 |
| `GET/DELETE/PATCH` | `/api/v1/sandboxes/{name}` | 获取/删除/更新沙箱 |
| `POST` | `/api/v1/sandboxes/execute` | 执行代码（代理到 Toolbox） |
| `GET/POST` | `/api/v1/sandboxes/stream` | 流式执行（SSE，代理到 Toolbox） |
| `GET` | `/api/v1/sandboxes/{name}/toolbox/*` | 直接代理到 Toolbox（文件/PTY/Session） |
| `GET` | `/api/v1/jobs/poll` | Poder Agent 轮询待处理 Job |
| `PATCH` | `/api/v1/jobs/{id}` | Poder Agent 更新 Job 状态 |

---

### 2.2 Poder（`cmd/poder`）

**职责**：
- 启动后主动连接 API Server WebSocket，建立 yamux 反向隧道
- 通过隧道暴露 HTTP 服务：接收 execute / file / PTY 请求，转发给容器内 Toolbox
- 轮询 `/api/v1/jobs/poll`，执行 CREATE / DELETE / START / STOP sandbox 任务
- 定时心跳上报容器使用量

**启动参数**：

| 参数 | 环境变量 | 默认值 | 说明 |
|------|----------|--------|------|
| `-api-url` | `API_URL` | `http://localhost:8080` | API Server 地址 |
| `-region` | `REGION` | `local` | 区域标识 |
| `-provider-type` | `PROVIDER_TYPE` | `local` | `docker` / `aws` / `aliyun` / `local` |
| `-poder-id` | `PODER_ID` | 自动生成 | Poder 唯一 ID（`poder-<容器ID前12位>`） |
| `-heartbeat-interval` | — | `10s` | 心跳间隔 |

**注意**：Poder 本身**不监听任何端口**，所有通信通过拨出的 WebSocket 隧道完成。

**Docker 启动示例**：

```bash
docker run -d --name sandrpod-poder \
  -v /var/run/docker.sock:/var/run/docker.sock \   # 或 ~/.docker/run/docker.sock
  --add-host host.docker.internal:host-gateway \
  sandrpod/poder:latest \
  -api-url=http://host.docker.internal:8080 \
  -region=local
```

---

### 2.3 sandrpod-agent（`cmd/agent`）

**职责**：
- 将**本地机器**直接注册为 SandrPod Sandbox，无需 Poder 或 Docker
- 内嵌 Toolbox，通过 WebSocket 反向隧道暴露给 API Server
- 适合 toC 场景：用户本机参与 AI 任务执行

**启动参数**：

| 参数 | 环境变量 | 说明 |
|------|----------|------|
| `-api-url` | `SANDRPOD_API_URL` | API Server 地址 |
| `-name` | `SANDRPOD_SANDBOX_NAME` | 沙箱名称（必须全局唯一） |
| `-work-dir` | `SANDRPOD_WORK_DIR` | 代码执行工作目录（默认当前目录） |
| `-token` | `SANDRPOD_TOKEN` | API 认证 token |
| `-reconnect` | — | 断线重连间隔（默认 5s） |

**注册后的沙箱特征**：

| 字段 | 值 |
|------|----|
| `provider_type` | `local-agent` |
| `proxy_url` | `direct://<sandbox-name>` |
| `state` | `RUNNING`（连接时）/ `ERROR`（断线时） |
| stop / start | **不支持**（生命周期由 agent 进程控制） |

**启动示例**：

```bash
sandrpod-agent \
  -api-url=https://api.sandrpod.io \
  -name=my-laptop \
  -work-dir=/home/user/projects
```

---

### 2.4 Toolbox（`cmd/toolbox`）

**职责**：运行在每个沙箱容器内，提供代码执行 HTTP API。

**实现语言**：Go（早期版本为 Flask/Python，当前版本为 Go）

**Toolbox HTTP API**（由 Poder 或 Agent 代理，客户端不直接访问）：

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/process` | 执行代码（python / bash / node） |
| `GET/POST` | `/stream` | 流式执行（SSE） |
| `GET` | `/pty/` | PTY 终端（WebSocket） |
| `POST` | `/process/session` | 创建有状态 session |
| `GET/POST/DELETE` | `/process/session/{id}` | 管理 session |
| `GET/POST/DELETE` | `/files/*` | 文件系统操作 |
| `GET` | `/health` | 健康检查 |
| `GET` | `/info` | 沙箱 OS/arch 信息 |

---

### 2.5 存储层（`pkg/store`）

采用 **Repository 模式**，接口定义在 `pkg/sandpod/repo.go`：

```
sandpod.SandboxRepository
sandpod.PoderRepository
sandpod.JobRepository
sandpod.Stores          ← 三个 Repository 的聚合，注入到所有 handler
```

**实现**：

| 包 | 后端 | 适用场景 |
|----|------|----------|
| `pkg/store/memory.go` | 进程内 map（加读写锁） | 开发 / 测试，重启即丢失 |
| `pkg/store/sqlite/` | SQLite（WAL 模式，modernc.org/sqlite） | 生产单节点，持久化 |

启动时通过 `-db` flag 选择：

```bash
# 内存模式（默认）
go run ./cmd/server

# SQLite 持久化
go run ./cmd/server -db sqlite:./data/sandrpod.db
```

PostgreSQL 实现（多实例水平扩展）见 [`design/horizontal-scaling.md`](design/horizontal-scaling.md)。

---

## 三、关键流程

### 3.1 Poder 注册与隧道建立

```
Poder                                    API Server
  │                                           │
  │── WS GET /ws/poder/connect ──────────────▶│
  │   Header: X-Poder-ID, X-Poder-Region,     │
  │           X-CPU-Cores, X-Memory-Bytes…    │
  │                                           │
  │◀─────────── 101 Switching Protocols ──────│
  │                                           │
  │◀══════════ yamux Session (双向) ══════════▶│
  │                                           │
  │  Poder 作为 yamux Server，                │  API Server 将 tunnel 存入
  │  在 session 上 Serve HTTP                 │  tunnelStore[poderID]
  │                                           │
  ├── 心跳 PUT /api/v1/poders/{id}/heartbeat ─▶│  更新 last_heartbeat + usage
  │   (每 10s，独立 HTTP 请求)                │
  │                                           │
  ├── 轮询 GET /api/v1/jobs/poll ─────────────▶│
  │◀────────────── [{job}, {job}] ────────────│
  │                                           │
  │  执行 Job (CREATE/DELETE/START/STOP)      │
  │  操作 Docker 完成后：                     │
  │                                           │
  ├── PATCH /api/v1/jobs/{id} ───────────────▶│  更新 Job 状态 + Sandbox 状态
```

### 3.2 创建 Sandbox（Poder 模式）

```
Client          API Server          Poder           Docker
  │                 │                 │               │
  │ POST /sandboxes │                 │               │
  ├────────────────▶│                 │               │
  │                 │ SelectBest()    │               │
  │                 │ 写 Job(PENDING) │               │
  │◀── 202 job_id ──│                 │               │
  │                 │                 │               │
  │                 │◀── PollJobs ────│               │
  │                 │─── [{job}] ────▶│               │
  │                 │                 │ docker run    │
  │                 │                 ├──────────────▶│
  │                 │                 │◀── container ─│
  │                 │ PATCH job       │               │
  │                 │◀───────────────��│               │
  │                 │ sandbox.State=RUNNING           │
  │ GET /sandboxes/{name}             │               │
  ├────────────────��│                 │               │
  │◀── {state:RUNNING, ip:…} ─────────│               │
```

### 3.3 sandrpod-agent 注册（直连模式）

```
sandrpod-agent                       API Server
       │                                  │
       │── WS GET /ws/sandbox/connect ───▶│
       │   Header: X-Sandbox-Name,        │
       │           X-Sandbox-Arch/OS       │
       │                                  │
       │◀──────── 101 Switching ──────────│
       │                                  │
       │◀═══ yamux Session ══════════════▶│ directStore[name] = tunnel
       │  Agent 作为 yamux Server          │ sandboxStore.Add({name, state:RUNNING,
       │  Serve toolbox HTTP              │   provider_type:"local-agent",
       │                                  │   proxy_url:"direct://name"})
```

### 3.4 执行代码（通用代理路径）

```
Client          API Server                  Poder/Agent    Toolbox
  │                 │                           │             │
  │ POST /execute   │                           │             │
  │ ?sandbox=foo    │                           │             │
  ├────────────────▶│ sandboxTunnel("foo")      │             │
  │                 │ → 查 sandboxStore         │             │
  │                 │ → 判断 proxy_url          │             │
  │                 │   tunnel:// → tunnelStore │             │
  │                 │   direct:// → directStore │             │
  │                 │                           │             │
  │                 │── yamux.Open() ──────────▶│             │
  │                 │── HTTP POST /execute ─────▶│             │
  │                 │                           │ POST /process
  │                 │                           ├────────────▶│
  │                 │                           │◀── output ──│
  │◀────── output ──│◀─────────────────────────│             │
```

---

## 四、目录结构

```
sandrpod/
├── cmd/
│   ├── server/          # API Server（控制平面 + 隧道代理）
│   ├── poder/           # Worker 节点（Docker 容器管理 + 轮询 Job）
│   ├── agent/           # 单机直连 Agent（toC 本地沙箱）
│   └── toolbox/         # 沙箱内代码执行服务
│
├── pkg/
│   ├── sandpod/         # 核心领域模型
│   │   ├── interface.go     # SandboxInfo, PoderInfo, Job 等数据结构
│   │   ├── repo.go          # Repository 接口 + Stores 聚合
│   │   ├── scheduler.go     # Poder 调度（SelectBest）
│   │   ├── sandbox_store.go # 内存 SandboxStore（遗留，被 store 层取代）
│   │   ├── poder_store.go   # 内存 PoderStore（遗留）
│   │   └── job_store.go     # 内存 JobStore（遗留）
│   │
│   ├── store/           # Repository 实现层
│   │   ├── interfaces.go    # sandpod.XxxRepository 的类型别名（向后兼容）
│   │   ├── memory.go        # 内存实现（包装 sandpod.*Store）
│   │   └── sqlite/          # SQLite 实现
│   │       ├── db.go            # Open()：WAL、pragma、启动恢复
│   │       ├── schema.go        # DDL + Migrate()
│   │       ├── sandbox_repo.go
│   │       ├── poder_repo.go    # SelectBest SQL 评分
│   │       └── job_repo.go      # PollJobs 原子事务
│   │
│   ├── poder/           # Poder 执行器接口 + Docker 实现
│   │   ├── interface.go     # PodExecutor 接口
│   │   ├── base.go
│   │   └── docker.go        # Docker SDK 实现
│   │
│   ├── provider/        # 云厂商抽象层
│   │   ├── interface.go     # Provider 接口（CreateVM / DeleteVM 等）
│   │   ├── factory.go       # 工厂模式注册
│   │   ├── aws/             # AWS EC2 实现
│   │   └── aliyun/          # 阿里云 ECS 实现
│   │
│   ├── toolbox/         # Toolbox HTTP 服务（Go）
│   │   ├── api.go           # HTTP handler
│   │   ├── executor.go      # 代码执行（python / bash / node）
│   │   ├── session.go       # 有状态 session（跨调用保持进程）
│   │   ├── session_api.go
│   │   ├── session_manager.go
│   │   ├── files.go         # 文件操作
│   │   └── pty_unix.go      # PTY 终端
│   │
│   └── tunnel/          # WebSocket + yamux 反向隧道
│       └── tunnel.go        # PoderTunnel, TunnelStore, wsConn
│
├── pkg/sdk/python/      # Python SDK + CLI（sandrpod-cli）
│
├── docker/
│   ├── Dockerfile.poder
│   └── Dockerfile.toolbox
│
├── docs/
│   ├── ARCHITECTURE.md          # 本文档（当前实现）
│   └── design/
│       ├── architecture-v1.md   # 历史设计规划（v1，部分未实现）
│       └── horizontal-scaling.md # 横向扩展方案（PostgreSQL + 多实例）
│
├── go.mod
├── go.sum
└── CLAUDE.md
```

---

## 五、部署快速参考

### 5.1 本地开发（内存模式）

```bash
# 启动 API Server
go run ./cmd/server -port 8080

# 启动 Poder（需要 Docker）
docker run -d --name sandrpod-poder \
  -v ~/.docker/run/docker.sock:/var/run/docker.sock \
  --add-host host.docker.internal:host-gateway \
  sandrpod/poder:test \
  -api-url=http://host.docker.internal:8080 -region=local

# 验证
sandrpod-cli --api-url http://localhost:8080 health
sandrpod-cli --api-url http://localhost:8080 create my-sb
```

### 5.2 持久化模式（SQLite）

```bash
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db
```

### 5.3 单机 Agent 模式（无 Docker）

```bash
# 将本机注册为沙箱
sandrpod-agent -api-url=http://localhost:8080 -name=my-laptop -work-dir=/tmp/work

# 然后直接在本机执行代码
sandrpod-cli --api-url http://localhost:8080 execute my-laptop "print('hello')" -l python
```

### 5.4 构建 Docker 镜像

```bash
docker build -f docker/Dockerfile.poder   -t sandrpod/poder:latest .
docker build -f docker/Dockerfile.toolbox -t sandrpod/toolbox:latest .
```

---

## 六、端口与环境变量汇总

| 组件 | 默认端口 | 说明 |
|------|----------|------|
| API Server | `:8080` | 唯一对外暴露的端口 |
| Poder | 无 | 不监听端口，通过拨出隧道通信 |
| sandrpod-agent | 无 | 同上 |
| Toolbox | `:8080`（容器内） | 仅容器内访问，测试时映射到 `:18080` |

| 组件 | 环境变量 | 说明 |
|------|----------|------|
| API Server | `TOKEN` | 认证 token（可选） |
| Poder | `API_URL` | API Server 地址 |
| Poder | `REGION` | 区域标识 |
| Poder | `PROVIDER_TYPE` | `docker` / `aws` / `aliyun` / `local` |
| sandrpod-agent | `SANDRPOD_API_URL` | API Server 地址 |
| sandrpod-agent | `SANDRPOD_SANDBOX_NAME` | 沙箱名称 |
| sandrpod-agent | `SANDRPOD_WORK_DIR` | 工作目录 |
