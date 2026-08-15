# SandrPod 架构文档

> **版本**: v0.5.6
> **更新日期**: 2026-08
> English: [ARCHITECTURE.md](ARCHITECTURE.md)
>
> 本文档描述当前**已实现并正常运行**的架构。历史设计规划见
> [`design/architecture-v1.md`](design/architecture-v1.md)，多实例部署见
> [`MULTI_INSTANCE_DEPLOYMENT.md`](MULTI_INSTANCE_DEPLOYMENT.md)。

---

## 一、系统概述

SandrPod 是面向 AI Agent 的自托管代码执行基础设施。设计建立在四点之上：

- **API Server 是唯一的控制平面，也是唯一的请求代理**，客户端只与它通信
- **Worker 主动拨出**。Poder 向 API Server 开一条 WebSocket 反向隧道，
  服务端顺着隧道下发请求——Worker 不暴露任何入站端口
- **sandrpod-agent 让任意机器直接成为沙箱**，不需要 Poder 也不需要 Docker，
  它自己注册并内嵌 Toolbox
- **Toolbox 跑在每个沙箱内**，提供代码执行 HTTP API

```
Client / SDK / CLI                    E2B SDK（未经修改）
        │  HTTP                              │  HTTPS
        ▼                                    ▼
┌──────────────────────────────────────────────────────────┐
│                     API Server  :8080                    │
│                                                          │
│  ┌────────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │  控制平面      │  │  隧道代理    │  │  E2B 网关    │  │
│  │  沙箱 CRUD     │  │  execute     │  │ api.<domain> │  │
│  │  job / poder   │  │  stream, PTY │  │ <port>-<id>. │  │
│  │  token         │  │  文件        │  │   <domain>   │  │
│  └────────────────┘  └──────────────┘  └──────────────┘  │
│                                                          │
│  ┌─────────────────┐  ┌────────────────────┐             │
│  │  tunnelStore    │  │  directStore       │             │
│  │  poderID→tunnel │  │  sandboxName→tunnel│             │
│  └─────────────────┘  └────────────────────┘             │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │  Store —— 内存 / SQLite / PostgreSQL               │  │
│  │  Sandbox · Poder · Job · Token · TunnelOwner       │  │
│  └────────────────────────────────────────────────────┘  │
└────────────┬──────────────────────────┬──────────────────┘
             │ WebSocket + yamux        │ WebSocket + yamux
             ▼                          ▼
   ┌──────────────────┐      ┌───────────────────────────┐
   │  Poder           │      │  sandrpod-agent           │
   │  (Docker Worker) │      │  (本机即沙箱)             │
   │                  │      │  内嵌 Toolbox             │
   │  ┌────────────┐  │      │  权限闸门 + 审计          │
   │  │  Toolbox   │  │      └─────────────┬─────────────┘
   │  │  (容器内)  │  │                    │ unix socket
   │  └────────────┘  │      ┌─────────────▼─────────────┐
   └──────────────────┘      │  sandrpod-tray            │
                             │  (用户会话 GUI)           │
                             └───────────────────────────┘
```

---

## 二、组件详解

### 2.1 API Server（`cmd/server`）

**职责**

- 控制平面：Sandbox / Poder / Job / API token 的 CRUD
- 隧道管理：接收 Poder 与 Agent 的 WebSocket 连接，维护 yamux 多路复用会话
- 请求代理：所有 execute / 文件 / PTY 请求都经隧道转发
- E2B 兼容网关（可选，按 Host 路由，见 §2.6）

**启动参数**

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-port` | `8080` | 监听端口 |
| `-token` | `""` | API token。空 = **不鉴权，匿名 admin**，切勿对外暴露 |
| `-tokens-file` | `""` | 命名 token JSON，热加载（见 `AUTH_AND_KEYS.md`） |
| `-db` | `""` | 存储后端：空 = 内存；`sqlite:<path>`；`postgres://…` |
| `-public-url` | `""` | 云 VM 回连的公网地址（云 provider 必填） |
| `-node-url` | `""` | 多实例模式下本实例的内部地址 |
| `-tls-cert` / `-tls-key` | `""` | 内建 TLS（直接 HTTPS 监听） |
| `-rate-limit` | `0` | 每 user token 限速（req/s，0 = 不限） |
| `-max-sandboxes-per-owner` | `0` | 每 owner 并发沙箱上限（0 = 不限） |
| `-offline-timeout` | `30s` | 心跳超时后标记 Poder 为 OFFLINE |
| `-reap-timeout` | `10m` | 回收 OFFLINE poder（终止云 VM） |
| `-sandbox-idle-timeout` | `0` | 回收闲置超时的沙箱（0 = 关闭） |
| `-poder-idle-timeout` | `0` | 回收无沙箱的云 poder（0 = 关闭） |

每个 flag 都以同名环境变量（`SANDRPOD_RATE_LIMIT`、
`SANDRPOD_SANDBOX_IDLE_TIMEOUT` …）作为默认值来源。E2B 网关则完全由环境变量配置
（`SANDRPOD_E2B_DOMAIN` 等）。完整清单见 `.env.example`。

**WebSocket 端点**

| 端点 | 用途 |
|---|---|
| `GET /ws/poder/connect` | Poder 拨入，注册并建立 yamux 隧道 |
| `GET /ws/sandbox/connect` | sandrpod-agent 拨入，直接注册为 Sandbox |

**HTTP API**

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/health` | 健康检查（同时返回版本号） |
| `GET` | `/metrics` | Prometheus 指标（admin） |
| `GET` | `/api/v1/poders` | 列出 Poder |
| `GET/DELETE` | `/api/v1/poders/{id}` | 获取 / 删除 Poder |
| `POST` | `/api/v1/sandboxes` | 创建沙箱（下发 Job） |
| `GET` | `/api/v1/sandboxes` | 列出沙箱 |
| `GET/DELETE/PATCH` | `/api/v1/sandboxes/{name}` | 获取 / 删除 / 更新 |
| `POST` | `/api/v1/sandboxes/{name}/start\|stop` | 生命周期 |
| `POST` | `/api/v1/sandboxes/execute` | 执行代码（代理到 Toolbox） |
| `GET/POST` | `/api/v1/sandboxes/stream` | 流式执行（SSE） |
| `GET` | `/api/v1/sandboxes/{name}/toolbox/*` | 通用 Toolbox 代理（文件 / PTY / session / watch） |
| `GET` | `/api/v1/jobs/poll` | Poder 轮询待处理 Job |
| `PATCH` | `/api/v1/jobs/{id}` | Poder 回报 Job 结果 |
| `GET/POST/DELETE` | `/api/v1/tokens…` | API token 签发与吊销（admin） |

---

### 2.2 Poder（`cmd/poder`）

**职责**

- 拨出 WebSocket 连到 API Server，建立 yamux 反向隧道
- 在隧道上提供 HTTP 服务：把 execute / 文件 / PTY 请求转发给目标容器内的 Toolbox
- 轮询 `/api/v1/jobs/poll`，执行 CREATE / DELETE / START / STOP 任务
- 定时心跳上报宿主机与容器用量

**启动参数**

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-api-url` | `API_URL` | `http://localhost:8080` | API Server 地址 |
| `-region` | `REGION` | `local` | 区域标识，调度器据此选点 |
| `-provider-type` | `PROVIDER_TYPE` | `local` | `local` / `docker` / 云厂商名 |
| `-poder-id` | `PODER_ID` | 自动生成 | 唯一 ID（`poder-<容器ID前缀>`） |
| `-network` | `SANDRPOD_NETWORK` | `""` | 沙箱容器接入的 Docker 网络 |
| `-token` | `SANDRPOD_TOKEN` | `""` | API token |
| `-vm-instance-id` | `VM_INSTANCE_ID` | `""` | 云实例 ID，用于 VM 回收 |
| `-heartbeat-interval` | — | `10s` | 心跳间隔 |

**Poder 不监听任何端口**，全部通信走它自己拨出的隧道。

```bash
docker run -d --name sandrpod-poder \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --add-host host.docker.internal:host-gateway \
  ghcr.io/sandrpod/poder:latest \
  -api-url=http://host.docker.internal:8080 -region=local
```

---

### 2.3 sandrpod-agent（`cmd/agent`）

把**本机**注册为沙箱——不需要 Poder，不需要 Docker。它内嵌 Toolbox 并通过反向
隧道暴露出去。这是员工机场景的路径，所以它同时承载权限闸门与审计管线（见 §2.7）。

**启动参数**

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-api-url` | `SANDRPOD_API_URL` | `http://localhost:8080` | API Server 地址 |
| `-name` | `SANDRPOD_SANDBOX_NAME` | — | 沙箱名（全局唯一） |
| `-work-dir` | `SANDRPOD_WORK_DIR` | 当前目录 | 代码执行工作目录 |
| `-token` | `SANDRPOD_TOKEN` | `""` | API token |
| `-reconnect` | — | `5s` | 断线重连间隔 |
| `-permission-mode` | `SANDRPOD_PERMISSION_MODE` | `off` | `off` / `prompt` / `strict` |
| `-permission-file` | `SANDRPOD_PERMISSION_FILE` | `~/.sandrpod/permissions.json` | 规则存储路径 |
| `-audit-dir` | `SANDRPOD_AUDIT_DIR` | `""` | 本地 NDJSON 审计日志目录（空 = 关闭） |
| `-audit-upload-url` | `SANDRPOD_AUDIT_UPLOAD_URL` | `""` | 批量上传端点 |
| `-audit-upload-token` | `SANDRPOD_AUDIT_UPLOAD_TOKEN` | 回退到 `-token` | 上传用 bearer token |
| `-mcp-enabled` | `SANDRPOD_MCP_ENABLED` | `false` | 启用 MCP 桥（见 §2.8） |
| `-mcp-config` | `SANDRPOD_MCP_CONFIG` | `~/.sandrpod/mcp.json` | MCP server 配置 |
| `-mcp-listen` | `SANDRPOD_MCP_LISTEN` | `127.0.0.1:7090` | MCP 桥监听地址 |
| `-mcp-token` | `SANDRPOD_MCP_TOKEN` | `""` | MCP 端点的 bearer token |
| `-mcp-only` | — | `false` | 只跑 MCP 桥，跳过沙箱注册 |
| `-mcp-oauth` | — | `false` | 为需要 OAuth 的 MCP server 启用授权 |
| `-mcp-oauth-callback` | `SANDRPOD_MCP_OAUTH_CALLBACK` | `127.0.0.1:7099` | OAuth 回调监听 |
| `-mcp-oauth-token-dir` | `SANDRPOD_MCP_OAUTH_TOKEN_DIR` | `~/.sandrpod/mcp-tokens` | token 存储目录 |
| `-mcp-grant-scope` | `SANDRPOD_MCP_GRANT_SCOPE` | `server` | 授权粒度：`server` / `tool` |
| `-mcp-guard-manifest` | — | `false` | 授权后拒绝工具定义漂移 |
| `-mcp-hot-reload` | — | `false` | 监视 MCP 配置变更并热加载 |

**注册后的沙箱特征**

| 字段 | 值 |
|---|---|
| `provider_type` | `local-agent` |
| `proxy_url` | `direct://<sandbox-name>` |
| `state` | 连接中 `RUNNING`，断线 `ERROR` |
| start / stop | **不支持**——生命周期归 agent 进程管 |

---

### 2.4 Toolbox（`cmd/toolbox`）

跑在每个沙箱内，提供执行 API，Go 编写。客户端不直接访问它，由 Poder 或 Agent 代理。

| 分组 | 路径 |
|---|---|
| 执行 | `POST /process` · `GET/POST /stream` |
| 有状态 session | `POST /process/session` · `/process/session/{id}` |
| 代码解释器 | `/code-interpreter/execute` · `/code-interpreter/contexts` · `/code-interpreter/contexts/{id}` |
| 进程管理 | `/procmgr/start` · `/stream` · `/input` · `/stdin-close` · `/signal` · `/resize` · `/list` |
| PTY | `GET /pty/`（WebSocket）· `POST /pty/create` |
| 文件 | `/files` · `/upload` · `/download` · `/bulk-upload` · `/delete` · `/move` · `/folder` · `/info` · `/find` · `/search` · `/replace` · `/permissions` · `/work-dir` · `/project-dir` · `/user-home-dir` |
| 目录监视 | `/watch/create` · `/watch/events` · `/watch/remove` |
| 端口代理 | `/proxy/<port>/*` → 沙箱内 `127.0.0.1:<port>` |
| MCP | `/mcp` · `/mcp/*` |
| 自省 | `/health` · `/status` · `/info` · `/metrics` |

**代码解释器**是 Jupyter 式的接口：一个 context 就是一份常驻内核命名空间，
变量跨调用保留。**端口代理**则是沙箱内起的 dev server 能被外部访问的原因。

---

### 2.5 存储层（`pkg/store`）

Repository 模式，接口定义在 `pkg/sandpod/repo.go`：

```
sandpod.SandboxRepository
sandpod.PoderRepository
sandpod.JobRepository
sandpod.TokenRepository
sandpod.TunnelOwnerRepository
sandpod.Stores            ← 注入到所有 handler 的聚合
```

| 包 | 后端 | 适用 |
|---|---|---|
| `pkg/store/memory.go` | 进程内 map + RWMutex | 开发 / 测试，重启即丢 |
| `pkg/store/sqldb/` | 方言参数化 SQL：SQLite（WAL，`modernc.org/sqlite`）**或** PostgreSQL（pgx 连接池，`FOR UPDATE SKIP LOCKED` 抢 job） | 生产 |

同一套代码同时支持两种后端。`sqldb/dialect.go` 处理占位符重写、DDL 类型和并发抢
job；具体用哪个，由启动时 `-db` 的 DSN scheme 选定，一个实例只用一种。

```bash
go run ./cmd/server                                    # 内存
go run ./cmd/server -db sqlite:./data/sandrpod.db      # SQLite，单实例
go run ./cmd/server -db "postgres://…?sslmode=require" # PostgreSQL，多实例
```

多实例能跑起来靠的是 `TunnelOwnerRepository`：它记录每个沙箱的隧道落在哪个节点，
于是请求落到任何一台都能转发到正确的那台。见
[`MULTI_INSTANCE_DEPLOYMENT.md`](MULTI_INSTANCE_DEPLOYMENT.md)。

---

### 2.6 E2B 兼容网关（`pkg/e2bcompat`）

让**未经修改的 E2B SDK** 直接跑在 SandrPod 上：把 `E2B_DOMAIN` 指向你的部署，
`Sandbox.create()` 就能用。不设 `SANDRPOD_E2B_DOMAIN` 则整个网关不启用。

路由按 Host 判定，决策点在 `cmd/server/e2bgateway.go`：

| Host | 交给谁 |
|---|---|
| `api.<domain>` 且路径属于 E2B 控制平面命名空间（`/sandboxes`、`/templates`、`/snapshots`、`/volumes`） | E2B 控制平面 |
| `api.<domain>` 的其余路径 | SandrPod 自己的 REST API |
| `<port>-<sandboxID>.<domain>` | E2B 数据平面 |

这个切分很关键：`api.<domain>` 是**共用**的。只有那四个 E2B 命名空间被分流，
所以开着 E2B 兼容层，`sandrpod-cli` 和原生 SDK 在同一个域名上照常工作。

`<port>-<sandboxID>.<domain>` 后面有三个数据平面接口。一个请求归谁处理，
**先看路径，再看端口**：

- **envd**——文件系统与进程 RPC（Connect/gRPC over protobuf），按路径前缀匹配：
  `/filesystem.*`、`/process.*`、`/files`。主机名里的端口不参与判定；E2B SDK 会在
  那里放 `49983`，但沙箱内并没有任何东西监听这个端口。
- **代码解释器**——`run_code`，匹配 `/execute` 与 `/contexts*`，或主机名端口等于
  `CodePort`（默认 `49999`）。底层是 Toolbox 的 context。
- **通用端口代理**——其余带端口的请求：经隧道反代到 Toolbox 的
  `/proxy/<port>/`，再由它拨到沙箱内 `127.0.0.1:<port>`。沙箱内的服务就是这样
  被访问到的——`50005` 上的 MCP 网关、用户自己起的 dev server、webhook 回调点。

两处授权细节值得留意：

- `Config.Authorize` **只**管数据平面。控制平面读共享存储、按 owner 过滤；
  数据平面的目标沙箱来自调用方给的 Host 或 header，不加这道检查的话，
  任何已认证的调用方都能凭 ID 访问别的租户的沙箱。
- `Config.PrivateSandboxPorts` 默认 **false**，与 E2B 一致：能猜到
  `<port>-<sandboxID>.<domain>` 这个主机名本身**就是**凭证。浏览器无法附加
  Authorization 头，强制要求它不是让常见用法变麻烦，而是变得不可能。
  只有当所有消费方都是能带 key 的程序时，才把它打开。

细节与已验证的功能矩阵见 [`E2B_COMPAT.md`](E2B_COMPAT.md)。

---

### 2.7 权限闸门与审计（`pkg/permission`、`pkg/notify`、`pkg/audit`）

opt-in，面向"沙箱就是某个人的笔记本"这种员工机部署。用 `-permission-mode` 开启，
默认 `off`，所以服务器部署完全不受影响。

**五分支决策**（`pkg/permission/manager.go`），先命中先返回：

1. **work_dir**——在 agent 工作目录内，静默放行
2. **hardlock**——拒绝。这一步排在任何放行规则查询**之前**，
   所以哪怕误给 `~/.ssh` 加了永久规则也绝不会生效
3. **永久规则**——用户添加的常驻授权
4. **会话授权**——带 TTL 的临时授权
5. **问人**——弹原生对话框；用户选"总是允许"就落盘

`pkg/notify` 按平台渲染这个对话框——macOS 用 `osascript`，Linux 用
`zenity`/`kdialog`，Windows 用 PowerShell `MessageBox`——并且**失败即拒绝**：
超时或出错一律当拒绝处理。

`pkg/audit` 把每次决策写进本地 NDJSON 日志（到 8 MiB 自动轮转）；配了上传地址
就在后台批量投递到中心端点，至少一次送达。它通过 `AuditSink` 接口与
`pkg/permission` 解耦。

**sandrpod-tray**（`cmd/sandrpod-tray`）是用户会话侧的伴随进程：托盘图标、
同意对话框、本地设置页。它经 `~/.sandrpod/authz.sock` 与 agent 通信。

```bash
sandrpod-tray serve                                # 托盘 + IPC + 设置页
sandrpod-tray rules ls                             # 列出永久规则与硬锁
sandrpod-tray rules add ~/Documents --mode rw
sandrpod-tray policy ls                            # 命令黑名单 / 警告名单
sandrpod-tray unlock ~/.ssh --i-understand-the-risk  # 只能从 CLI，GUI 里没有
sandrpod-tray seed                                 # 安装默认硬锁
```

完整说明见 [`PERMISSION_AND_AUDIT.md`](PERMISSION_AND_AUDIT.md)。

---

### 2.8 MCP 桥（`pkg/mcpbridge`、`cmd/mcp-gateway`）

把多个 MCP server 聚合到一个 Streamable-HTTP 端点后面，agent 拿到的是一份统一的
工具命名空间，而不是 N 条连接。`pkg/mcpbridge` 负责子进程监管（拉起、退避、重启），
并用 `SplitFQName` 那套前缀规则合并各家的工具列表。

两种部署形态：

- **跑在 agent 里**——`sandrpod-agent -mcp-enabled`，监听 `127.0.0.1:7090`。
  子 server 在用户机器上用用户的凭据运行，权限闸门经 `PermissionGate` 生效。
- **跑在沙箱里**——`cmd/mcp-gateway` 在容器内监听 `50005`，经 E2B 端口代理访问。
  同时接受 E2B 的 `mcp` map 格式和 sandrpod 的 `{mcpServers:{…}}` 格式。

`-mcp-oauth` 处理需要 OAuth 的 server（如 Notion）：授权 URL 只经 admin socket
呈现，绝不发给远端调用方。`-mcp-guard-manifest` 在授权后钉住工具定义，
某个 server 偷偷改了工具 schema 会被拒绝而不是被信任。

桥同时挂在 `~/.sandrpod/mcp-local.sock` 这个 AF_UNIX socket 上，与隧道入口
共用同一个 manager——同一台机器上的宿主可以直连，不必出网再绕回来经控制平面
（0.13 毫秒 vs 约 1 秒）。别名命名空间、权限门、审计全部一致：这是快车道，
不是旁路。认证边界是 socket 的 0600 权限，所以刻意不加 token。

**资源**与工具一同被代理，这是
[MCP Apps](https://modelcontextprotocol.io/seps/1865-mcp-apps-interactive-user-interfaces-for-mcp)
宿主能取到界面 HTML 的前提：那份 HTML 只能经 `resources/read` 拿到，而约束
iframe 的 CSP 与权限声明挂在资源的 `_meta` 上，所以桥把上游内容原样返回。
URI 的命名空间做在 authority 段（`ui://form` → `ui://<alias>/form`）——否则两个
都提供 `ui://form` 的 server 会直接撞车——同时把各工具的 `_meta.ui.resourceUri`
改写成一致的值。不提供资源的 server 根本不会被问。

见 [`MCP_BRIDGE.md`](MCP_BRIDGE.md) 与 [`MCP_AUTH.md`](MCP_AUTH.md)。

---

## 三、状态机

```
PENDING → STARTING → RUNNING → STOPPING → STOPPED
                        │                    │
                        └──────► ERROR ◄─────┘
                                   │
                                   ▼
                              TERMINATED
```

定义在 `pkg/sandpod/interface.go`。`TERMINATED` 是终态。agent 注册的沙箱只会在
`RUNNING` 和 `ERROR` 之间切换。

---

## 四、关键流程

### 4.1 Poder 注册与隧道建立

```
Poder                                    API Server
  │                                           │
  │── WS GET /ws/poder/connect ──────────────▶│
  │   Header: X-Poder-ID, X-Poder-Region,     │
  │           X-CPU-Cores, X-Memory-Bytes…    │
  │                                           │
  │◀─────────── 101 Switching Protocols ──────│
  │                                           │
  │◀══════════ yamux 会话（双向）════════════▶│
  │                                           │
  │  Poder 作为 yamux *服务端*，              │  服务器把 tunnel 存进
  │  在会话上 Serve HTTP                      │  tunnelStore[poderID]
  │                                           │
  ├── PUT /api/v1/poders/{id}/heartbeat ─────▶│  更新 last_heartbeat + 用量
  │   （每 10s，独立 HTTP 请求）              │
  │                                           │
  ├── GET /api/v1/jobs/poll ─────────────────▶│
  │◀────────────── [{job}, {job}] ────────────│
  │                                           │
  │  执行 Job（CREATE/DELETE/START/STOP）     │
  │                                           │
  ├── PATCH /api/v1/jobs/{id} ───────────────▶│  更新 Job 与 Sandbox 状态
```

### 4.2 创建 Sandbox（Poder 路径）

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
  │                 │◀────────────────│               │
  │                 │ sandbox.State = RUNNING         │
  │ GET /sandboxes/{name}             │               │
  ├────────────────▶│                 │               │
  │◀── {state:RUNNING, ip:…} ─────────│               │
```

### 4.3 sandrpod-agent 注册（直连路径）

```
sandrpod-agent                       API Server
       │                                  │
       │── WS GET /ws/sandbox/connect ───▶│
       │   Header: X-Sandbox-Name,        │
       │           X-Sandbox-Arch/OS      │
       │                                  │
       │◀──────── 101 Switching ──────────│
       │                                  │
       │◀═══ yamux 会话 ═════════════════▶│ directStore[name] = tunnel
       │  agent 作为 yamux 服务端，       │ store.Add({name, state:RUNNING,
       │  Serve toolbox HTTP API          │   provider_type:"local-agent",
       │                                  │   proxy_url:"direct://name"})
```

### 4.4 执行代码（通用代理路径）

```
Client          API Server                  Poder/Agent    Toolbox
  │                 │                           │             │
  │ POST /execute   │                           │             │
  │ ?sandbox=foo    │                           │             │
  ├────────────────▶│ sandboxTunnel("foo")      │             │
  │                 │ → 查沙箱记录              │             │
  │                 │ → 按 proxy_url 分派       │             │
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

### 4.5 一次 E2B SDK 请求

```
e2b.Sandbox                    API Server                  Toolbox
  │                                │                          │
  │ POST api.<domain>/sandboxes    │                          │
  ├───────────────────────────────▶│ host==api 且属 E2B 路径  │
  │                                │ → E2B 控制平面           │
  │◀── {sandboxID, …} ─────────────│ → 走调度器创建           │
  │                                │                          │
  │ POST 49999-<id>.<domain>/execute                          │
  ├───────────────────────────────▶│ host 命中 envd 模式      │
  │                                │ → 从 Host 解析沙箱，     │
  │                                │   过 Authorize()         │
  │                                │ → 进隧道                 │
  │                                ├─────────────────────────▶│
  │                                │   /code-interpreter/execute
  │◀────── 流式结果 ───────────────│◀─────────────────────────│
```

---

## 五、目录结构

```
sandrpod/
├── cmd/
│   ├── server/          # API Server：控制平面、隧道代理、E2B 网关
│   ├── poder/           # Worker：Docker 生命周期 + Job 轮询
│   ├── agent/           # 直连 agent（本机即沙箱）
│   ├── toolbox/         # 沙箱内执行服务
│   ├── sandrpod-tray/   # 用户会话 GUI 伴随进程（需 CGO：Cocoa/GTK/win32）
│   └── mcp-gateway/     # 沙箱内 MCP 聚合器（端口 50005）
│
├── pkg/
│   ├── sandpod/         # 核心领域模型
│   │   ├── interface.go     # SandboxInfo、PoderInfo、Job、State
│   │   ├── repo.go          # Repository 接口 + Stores 聚合
│   │   ├── scheduler.go     # Poder 调度（SelectBest）
│   │   └── *_store.go       # 内存 store（遗留，被 pkg/store 包装）
│   │
│   ├── store/           # Repository 实现层
│   │   ├── memory.go        # 内存适配
│   │   └── sqldb/           # 一套代码同时支持 SQLite 与 PostgreSQL
│   │       ├── dialect.go       # 占位符重写、DDL 类型、抢 job
│   │       ├── db.go            # Open()、pragma、启动恢复
│   │       ├── schema.go        # DDL + Migrate()
│   │       ├── sandbox_repo.go  poder_repo.go  job_repo.go
│   │       ├── token_repo.go    # API token（只存 hash）
│   │       └── tunnelowner_repo.go  # 隧道归属哪个节点
│   │
│   ├── e2bcompat/       # E2B 线协议网关
│   │   ├── gateway.go       # Config、Handler、Host 路由、授权
│   │   ├── controlplane.go  # /sandboxes、/templates、/snapshots、/volumes
│   │   ├── envd.go          # 文件系统 + 进程 RPC（Connect/gRPC）
│   │   ├── process.go  protobuf.go  protobuf_process.go
│   │   ├── codeinterp.go    # run_code 接口
│   │   ├── watch.go         # 目录监视流
│   │   └── apikey.go        # e2b_<hex> 密钥生成与查找
│   │
│   ├── permission/      # 员工机决策引擎（五分支策略）
│   ├── notify/          # 原生同意对话框；失败即拒绝
│   │   └── prompt_{darwin,linux,windows}.go
│   ├── audit/           # NDJSON 记录器 + 后台批量上传
│   ├── mcpbridge/       # MCP 子进程监管、工具聚合、OAuth
│   │
│   ├── poder/           # Pod 执行器接口 + Docker 实现
│   ├── provider/        # 云厂商抽象层
│   │   ├── interface.go  factory.go
│   │   ├── aws/ aliyun/ azure/ tencent/ oracle/   # 托管 run-command
│   │   ├── gcp/ digitalocean/ hetzner/            # SSH
│   │   └── sshexec/         # 共享 SSH 执行器（DO/Hetzner）
│   │
│   ├── toolbox/         # 沙箱内 HTTP 服务
│   │   ├── api.go  executor.go  files.go  pty_unix.go
│   │   ├── procmgr.go       # 进程管理
│   │   ├── session*.go      # 有状态 session
│   │   └── watch.go         # 目录监视
│   │
│   ├── tunnel/          # WebSocket + yamux 反向隧道
│   ├── logging/  brand/  homedir/                 # 共享基础设施
│   └── sdk/python/      # Python SDK + sandrpod-cli + langchain-sandrpod
│
├── docker/              # Dockerfile + 参考 compose
└── docs/                # 本文档；索引见 docs/README.md
```

---

## 六、端口与环境变量

| 组件 | 端口 | 说明 |
|---|---|---|
| API Server | `:8080` | 唯一对客户端暴露的端口 |
| Poder | 无 | 拨出，不监听 |
| sandrpod-agent | 无 | 同上 |
| Toolbox | 容器内 `:8080` | 只能经隧道访问（测试时映射到 `:18080`） |
| MCP 桥（agent） | `127.0.0.1:7090` | 仅回环 |
| MCP 本机 socket | `~/.sandrpod/mcp-local.sock` | 同机宿主直连；0600，无 token |
| mcp-gateway（沙箱） | `:50005` | 经 E2B 端口代理访问 |

| 组件 | 环境变量 | 用途 |
|---|---|---|
| API Server | `SANDRPOD_TOKEN` | API token |
| API Server | `SANDRPOD_E2B_DOMAIN` | 启用 E2B 网关，并指定基础域名 |
| Poder | `API_URL` / `REGION` / `PROVIDER_TYPE` | 服务器地址、区域、provider |
| Poder | `SANDRPOD_TOOLBOX_IMAGE` | 沙箱镜像 |
| sandrpod-agent | `SANDRPOD_API_URL` / `SANDRPOD_SANDBOX_NAME` / `SANDRPOD_WORK_DIR` | 地址、名称、工作目录 |
| sandrpod-agent | `SANDRPOD_PERMISSION_MODE` / `SANDRPOD_AUDIT_*` | 权限闸门与审计 |

含默认值的完整清单见 `.env.example`。

---

## 七、部署快速参考

```bash
# 本地开发（内存存储）
go run ./cmd/server -port 8080
docker run -d --name sandrpod-poder \
  -v ~/.docker/run/docker.sock:/var/run/docker.sock \
  --add-host host.docker.internal:host-gateway \
  ghcr.io/sandrpod/poder:latest \
  -api-url=http://host.docker.internal:8080 -region=local

# 持久化，单实例
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db

# Agent 模式——不需要 Docker，本机就是沙箱
sandrpod-agent -api-url=http://localhost:8080 -name=my-laptop -work-dir=/tmp/work
sandrpod-cli execute my-laptop "print('hello')" -l python
```

真实部署——PostgreSQL、通配 TLS、开启 E2B 接口，以及证明它确实能用的验收扫描——
按 [`PRODUCTION_DEPLOYMENT.md`](PRODUCTION_DEPLOYMENT.md) 走。
