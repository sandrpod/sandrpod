# SandrPod MCP Transport Bridge 设计方案

> **状态**: 设计稿 v0.1
> **更新日期**: 2026-05-27
> **目标仓库**: `github.com/sandrpod/sandrpod`（独立产品，最终落地于该仓库的 `pkg/mcpbridge/` 与 `cmd/agent`）
> **本文存放位置**: sandrpod `docs/`（设计源 / 实施依据）；Acme `devdocs/SANDRPOD_MCP_BRIDGE_DESIGN.md` 维护一份镜像副本供消费方平台团队参考
> **核心定位**: 让 SandrPod 节点（poder / agent）成为本机 stdio MCP 服务器的**远程出口**，对外暴露**单一标准 Streamable HTTP MCP endpoint**，复用 Claude/Cursor/Cline 等工具通用的 `mcp.json` 配置格式
>
> 受影响的包：新增 `pkg/mcpbridge`；改动 `cmd/agent`、`cmd/server`、`pkg/permission`、`pkg/audit`、`cmd/sandrpod-tray`

---

## 目录

- [一、为什么要做](#一为什么要做)
- [二、设计目标与非目标](#二设计目标与非目标)
- [三、架构总览](#三架构总览)
- [四、配置格式：复用 Claude `mcp.json`](#四配置格式复用-claude-mcpjson)
- [五、新增包 `pkg/mcpbridge`](#五新增包-pkgmcpbridge)
- [六、`cmd/agent` 接入](#六cmdagent-接入)
- [七、`cmd/server` 转发路由](#七cmdserver-转发路由)
- [八、与 `pkg/permission` 集成](#八与-pkgpermission-集成)
- [九、与 `pkg/audit` 集成](#九与-pkgaudit-集成)
- [十、与 `cmd/sandrpod-tray` 集成](#十与-cmdsandrpod-tray-集成)
- [十一、独立使用模式（无 tunnel）](#十一独立使用模式无-tunnel)
- [十二、消费方对接](#十二消费方对接)
- [十三、错误处理与离线降级](#十三错误处理与离线降级)
- [十四、安全模型与已知限制](#十四安全模型与已知限制)
- [十五、实施路径](#十五实施路径)

---

## 一、为什么要做

SandrPod 当前的能力边界停在「让远程 AI agent 在本机执行 shell / 文件 / PTY」。

但 AI agent 生态中另一类核心连接器——**MCP (Model Context Protocol) server**——目前仍只能跑在 AI 后端进程旁边（通过 stdio fork-exec）或部署到企业 IT 控制的服务器上。普通员工无法在自己电脑上装一个「连我个人 GitHub / 我的 Notion / 我的本地 Figma 项目」的 MCP，让远端 AI 直接用上。

这导致两个产品形态的鸿沟：

| 形态 | 现状 | 问题 |
|---|---|---|
| **桌面型 AI** (Claude Desktop / Cursor / Cline) | 本机 LLM client + 本机 stdio MCP，配 `mcp.json` 即可 | 只能在桌面跑，不能跨设备、不能给中心化平台用 |
| **企业平台型 AI** (Acme / Dify / 自研编排器) | 集中编排，能跨渠道，但 MCP 只能后端配 | 员工没有自助权，凭据要进 IT 系统 |

**MCP Transport Bridge 想做的事**：把员工 PC 上「装一个 stdio MCP」变成「远端 AI 也能用」。**用 SandrPod 已有的反向隧道把 stdio MCP 透传成标准的 Streamable HTTP MCP endpoint**——任何符合 MCP 规范的 AI 编排器都能直接接入，无需感知它跑在远端 PC 上。

这一能力对非 Acme 消费方同样有价值：

- **任何 LangChain / DeepAgents / OpenAI Function Calling 编排器** 都可以把 SandrPod 暴露的 HTTP MCP 直接当远程 MCP 来用
- **多用户 SaaS AI 工具** 想为每个用户提供「带个人凭据的 MCP」时，可以让用户安装 sandrpod-agent，自己的 mcp.json 留在自己电脑上
- **MCP 开发者** 测试 stdio MCP 时，可以直接通过 sandrpod 暴露成远程 endpoint 给其它工具调用，不必每个工具都跑一份 stdio fork

---

## 二、设计目标与非目标

### 设计目标

1. **复用业界标准**：`mcp.json` 格式与 Claude Desktop / Cursor 完全一致，员工可直接复制现有配置
2. **零协议私造**：所有对外接口走标准 MCP Streamable HTTP transport（spec 2025-03-26+），消费方使用任何 MCP client SDK 即可对接
3. **单一外部端点**：N 个本地 stdio MCP server 聚合为**一个**对外 HTTP MCP endpoint，N→1 收敛连接数与命名空间
4. **凭据本地化**：`mcp.json` 中 `env` 块的 token / API key 仅存在于员工 PC 进程环境变量，**永不跨网络**
5. **复用 SandrPod 隧道**：使用现有 `pkg/tunnel` 反向 WebSocket+yamux，无需新协议、无需开放员工 PC 入站端口
6. **嵌入式可用**：`pkg/mcpbridge` 可作为 Go library 被任何 sandrpod 衍生项目或第三方工具直接使用
7. **可独立运行**：支持 `sandrpod-agent --mcp-only --listen :7090` 不走隧道的本地 LAN 模式

### 非目标

1. **不重新实现 stdio MCP server**：所有 `npx @modelcontextprotocol/server-*`、Python `mcp-server-*` 等社区 MCP 服务器保持原样运行
2. **不做 MCP 协议扩展**：聚合器对外严格遵循 MCP 规范，不引入私有方法/字段
3. **不替代企业级中心化 MCP**：MCP Bridge 解决的是「个人 PC 上的 MCP」；企业级中心化 MCP server 仍由消费方自行托管
4. **不做 MCP server marketplace**：发现/安装 UX 是 sandrpod-tray 或消费方的产品工作，本设计只提供运行时

---

## 三、架构总览

```
┌─────────────────────────────── 员工 PC ───────────────────────────────┐
│                                                                       │
│  ~/.sandrpod/mcp.json   (Claude Desktop 兼容格式)                     │
│         │                                                             │
│         ▼                                                             │
│  ┌──────────────────── sandrpod-agent ───────────────────┐            │
│  │                                                        │            │
│  │  ┌─────────── pkg/mcpbridge ────────────┐              │            │
│  │  │                                       │              │            │
│  │  │  ChildManager                         │              │            │
│  │  │    ├─ spawn github  (stdio child)     │              │            │
│  │  │    ├─ spawn jira    (stdio child)     │              │            │
│  │  │    ├─ spawn notion  (stdio child)     │              │            │
│  │  │    └─ ...                              │              │            │
│  │  │                                       │              │            │
│  │  │  Aggregator                           │              │            │
│  │  │    ├─ implements MCP server protocol  │              │            │
│  │  │    ├─ tools/list → union with prefix  │              │            │
│  │  │    │  (github__list_issues, jira__... )│              │            │
│  │  │    ├─ tools/call → route by prefix    │              │            │
│  │  │    │  to right stdio child            │              │            │
│  │  │    └─ notifications: child up/down    │              │            │
│  │  │       → tools/list_changed            │              │            │
│  │  │                                       │              │            │
│  │  │  HTTPHandler (Streamable HTTP)        │              │            │
│  │  │    POST /mcp   POST /mcp/sse  ...     │              │            │
│  │  └────────────────┬──────────────────────┘              │            │
│  │                   │                                     │            │
│  │  newAgentMux:     │                                     │            │
│  │    /mcp → bridge ─┘                                     │            │
│  │    /execute → toolbox …                                 │            │
│  │    /files/… → toolbox …                                 │            │
│  │                                                         │            │
│  └─────────────────────┬───────────────────────────────────┘            │
│                        │                                                │
│                        ▼ yamux stream over WebSocket                   │
└───────────────────────────────────────────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────── API Server ────────────────────────────┐
│                                                                       │
│  pkg/tunnel.PoderTunnel (existing)                                    │
│         │                                                             │
│         ▼                                                             │
│  cmd/server: proxyHTTP                                                │
│    /api/v1/sandboxes/{name}/mcp         → tunnel /mcp                 │
│    /api/v1/sandboxes/{name}/mcp/manifest → bridge metadata (synthetic)│
└───────────────────────┬───────────────────────────────────────────────┘
                        │
                        ▼
            Any MCP-compatible AI orchestrator
            (Acme / LangChain / Dify / OpenAI / …)
```

### 关键设计取舍

| 决策 | 选择 | 原因 |
|---|---|---|
| 暴露端口数 | **单 endpoint** + 工具名前缀 | 后端 MCP 注册表 1 行/sandbox，不是 N 行 |
| MCP transport | **Streamable HTTP**（含 SSE 模式） | 标准；隧道天然转发 HTTP；client 选择灵活 |
| 子进程实现 | **官方 stdio MCP child**，不重写 | 兼容生态 |
| 进程隔离 | **每个 child 独立 OS 进程** | 崩溃隔离 + env 变量隔离 |
| 命名空间 | `<server>__<tool>` 双下划线分隔 | Anthropic 工具命名约定；可读性好 |
| 凭据落地 | 子进程 env 变量，**不持久化** | 不写日志、不写审计、`mcp.json` 由员工自管 |

---

## 四、配置格式：复用 Claude `mcp.json`

### 默认位置

| OS | 路径 |
|---|---|
| macOS | `~/.sandrpod/mcp.json` |
| Linux | `~/.sandrpod/mcp.json`（XDG: `$XDG_CONFIG_HOME/sandrpod/mcp.json`） |
| Windows | `%APPDATA%\sandrpod\mcp.json` |

可通过 `--mcp-config=/some/path/mcp.json` 显式指定。员工可直接 `cp ~/Library/Application\ Support/Claude/claude_desktop_config.json ~/.sandrpod/mcp.json`。

### 格式（Claude Desktop 兼容）

```json
{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_xxxxxxxxxx"
      }
    },
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/me/projects"]
    },
    "notion": {
      "command": "python",
      "args": ["-m", "mcp_server_notion"],
      "env": { "NOTION_TOKEN": "secret_xxxxxxxxxx" }
    }
  }
}
```

### SandrPod 扩展字段（可选，向后兼容）

为支持 sandrpod 特有的运行时控制，额外允许（位于每个 server 配置下）：

```jsonc
{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_..." },

      // ── 以下为 SandrPod 扩展，缺省值见注释 ──
      "sandrpod": {
        "enabled": true,             // false 时跳过该 server
        "alias": "gh",               // 工具前缀别名（默认用 key 本身）
        "restart_policy": "always",  // always | on-failure | never
        "max_restart_per_min": 3,
        "startup_timeout_sec": 30,
        "tool_allowlist": ["list_issues", "create_pr"],  // 不在列表内的工具不暴露
        "tool_denylist": ["delete_repo"]
      }
    }
  }
}
```

**关键约束**：扩展字段全部位于 `sandrpod` 子对象内，确保 mcp.json 仍可被 Claude Desktop 等工具直接读取（它们会忽略未知字段）。

---

## 五、新增包 `pkg/mcpbridge`

### 包职责

`pkg/mcpbridge` 提供 **stdio→HTTP MCP 聚合器**的可复用实现。不依赖 `cmd/agent`，可独立 import 到任何 Go 项目。

### 文件布局

```
pkg/mcpbridge/
├── doc.go               // 包级别文档
├── config.go            // mcp.json 解析（含 SandrPod 扩展）
├── child.go             // 单个 stdio child 进程封装（生命周期、JSON-RPC over stdio）
├── manager.go           // ChildManager：多 child 调度、热加载、健康检查
├── aggregator.go        // MCP 协议层：tools/list、tools/call、prompts、resources 聚合
├── handler.go           // Streamable HTTP transport 实现
├── manifest.go          // 元信息端点 /mcp/manifest（非 MCP 标准，是 sandrpod 增值）
├── permission.go        // 权限网关 hook 接口（不直接依赖 pkg/permission）
├── audit.go             // 审计 hook 接口（不直接依赖 pkg/audit）
└── *_test.go
```

### 核心类型

```go
// pkg/mcpbridge/config.go

type Config struct {
    McpServers map[string]ServerConfig `json:"mcpServers"`
}

type ServerConfig struct {
    Command string            `json:"command"`
    Args    []string          `json:"args,omitempty"`
    Env     map[string]string `json:"env,omitempty"`

    Sandrpod *SandrpodOpts    `json:"sandrpod,omitempty"`
}

type SandrpodOpts struct {
    Enabled          bool     `json:"enabled,omitempty"`
    Alias            string   `json:"alias,omitempty"`
    RestartPolicy    string   `json:"restart_policy,omitempty"`
    MaxRestartPerMin int      `json:"max_restart_per_min,omitempty"`
    StartupTimeoutSec int     `json:"startup_timeout_sec,omitempty"`
    ToolAllowlist    []string `json:"tool_allowlist,omitempty"`
    ToolDenylist     []string `json:"tool_denylist,omitempty"`
}
```

```go
// pkg/mcpbridge/child.go

type Child struct {
    Name   string                 // mcp.json 里的 key（如 "github"）
    Alias  string                 // 工具前缀（默认 == Name）
    Cfg    ServerConfig
    cmd    *exec.Cmd
    stdin  io.WriteCloser
    stdout *bufio.Scanner
    pending sync.Map             // request id → channel
    tools  []ToolDescriptor      // 缓存的 tools/list 结果
    state  ChildState            // starting / ready / failed / restarting
    mu     sync.RWMutex
}

func (c *Child) Start(ctx context.Context) error { /* ... */ }
func (c *Child) Stop(ctx context.Context) error  { /* ... */ }
func (c *Child) Call(ctx context.Context, method string, params any) (json.RawMessage, error) { /* ... */ }
func (c *Child) ListTools(ctx context.Context) ([]ToolDescriptor, error) { /* ... */ }
```

**关键点**：

- `Child` 与子进程的通信走标准 **MCP stdio transport**（JSON-RPC 2.0 over newline-delimited JSON），完全符合 MCP spec，不做任何协议改造。
- `pending` 字典记录每个 outbound request id 对应的 channel，stdout reader 协程统一 dispatch，避免阻塞。
- `Start()` 阶段执行一次 `initialize` + `tools/list`，缓存工具清单。
- crash 检测：stdout 关闭 → 走 `restart_policy` 决定是否重启；超过 `max_restart_per_min` 进入 `failed` 永久状态。

```go
// pkg/mcpbridge/manager.go

type ChildManager struct {
    cfgPath  string
    children map[string]*Child
    watcher  *fsnotify.Watcher  // mcp.json 热加载
    perm     PermissionGate     // 可选注入
    audit    AuditSink          // 可选注入
    mu       sync.RWMutex
    onChange []func()           // 触发 tools/list_changed 用
}

func NewManager(opts ManagerOptions) *ChildManager
func (m *ChildManager) Start(ctx context.Context) error
func (m *ChildManager) Reload(ctx context.Context) error           // 由 fsnotify / 显式 API 调
func (m *ChildManager) Aggregate() ([]ToolDescriptor, error)
func (m *ChildManager) Dispatch(ctx context.Context, fqTool string, args json.RawMessage) (json.RawMessage, error)
func (m *ChildManager) OnChange(fn func())
```

```go
// pkg/mcpbridge/aggregator.go

type Aggregator struct {
    mgr *ChildManager
}

// Aggregator 对外实现一个完整的 MCP server（initialize / tools / resources / prompts / notifications）
// 这里只列工具相关；resources / prompts 同模式

func (a *Aggregator) HandleToolsList(ctx context.Context) (*ToolsListResult, error) {
    tools, _ := a.mgr.Aggregate()
    // 为每个 tool 加前缀: alias + "__" + tool_name
    // 同名工具按 mcp.json 中 server 出现顺序后到的"后缀化"为 _2/_3 etc.
    return &ToolsListResult{Tools: tools}, nil
}

func (a *Aggregator) HandleToolsCall(ctx context.Context, fqName string, args json.RawMessage) (*ToolsCallResult, error) {
    // 1. 拆前缀：fqName = "github__list_issues" → alias="github", tool="list_issues"
    // 2. 调 ChildManager.Dispatch 到对应 child
    // 3. 透传结果
}
```

```go
// pkg/mcpbridge/handler.go

// HTTPHandler 提供 MCP Streamable HTTP transport 实现
// 路由约定：
//   POST /mcp       — JSON-RPC over plain HTTP (request/response)
//   POST /mcp       — 同上，但 Accept: text/event-stream 时升级为 SSE（spec 推荐）
//   GET  /mcp/sse   — 老式 SSE-only 模式（spec 2024 版兼容）
//   GET  /mcp/manifest — sandrpod 扩展：当前加载的 server 清单 + 健康状态

func NewHTTPHandler(agg *Aggregator) http.Handler
```

### 核心命名规则

工具完全限定名格式：`<alias>__<tool_name>`

- alias 默认为 mcp.json 中的 server key
- 子工具名内出现 `__` 不冲突（split 只用第一个 `__`）
- 长度限制：alias 最多 16 字符，tool_name 由 child 决定。如果 alias+tool 长度超过 OpenAI/Anthropic API 的 64 字符限制，alias 自动截断 + hash 后缀（实现：SHA1(alias)[0:6]）

### `/mcp/manifest` 扩展端点

非 MCP 标准的元信息端点，供 SandrPod 控制面（或第三方监控）查询当前状态：

```json
{
  "schema_version": 1,
  "loaded_at": "2026-05-27T10:30:00Z",
  "servers": [
    {
      "name": "github",
      "alias": "github",
      "state": "ready",
      "command": "npx",
      "version": "0.6.2",
      "tool_count": 8,
      "started_at": "2026-05-27T10:30:01Z",
      "restart_count": 0
    },
    {
      "name": "jira",
      "state": "failed",
      "last_error": "exit code 1: missing JIRA_TOKEN env",
      "restart_count": 3
    }
  ],
  "total_tools": 24
}
```

env 字段、token 等敏感信息**永不出现**在 manifest 输出里。

### 协议转换实现：库 vs 自研代码的分工

这是本设计的核心工程问题——把员工 PC 上的 **stdio MCP** 透传成 **HTTP MCP**，到底哪些是现成轮子、哪些必须自己写。

#### 协议两端的本质

```
┌──────────────────── stdio 子进程侧 ────────────────────┐
│                                                        │
│  child stdin:   {"jsonrpc":"2.0","id":7,"method":      │
│                  "tools/call","params":{...}}\n        │
│                                                        │
│  child stdout:  {"jsonrpc":"2.0","id":7,"result":      │
│                  {"content":[...]}}\n                  │
│                                                        │
│  Transport: newline-delimited JSON (NDJSON)            │
│  Spec: MCP 2025-03-26 §Transports → stdio              │
└────────────────────────────────────────────────────────┘
                        ↕  Bridge
┌──────────────────── HTTP endpoint 侧 ──────────────────┐
│                                                        │
│  POST /mcp                                             │
│  Content-Type: application/json                        │
│  Accept: application/json, text/event-stream           │
│                                                        │
│  Body: {"jsonrpc":"2.0","id":7,"method":...}           │
│                                                        │
│  Response:                                             │
│    A) Content-Type: application/json (one-shot)        │
│       Body: {"jsonrpc":"2.0","id":7,"result":...}      │
│    B) Content-Type: text/event-stream (streaming)      │
│       data: {"jsonrpc":"2.0",...}\n\n                  │
│       data: {"jsonrpc":"2.0",...}\n\n                  │
│                                                        │
│  Spec: MCP 2025-03-26 §Transports → Streamable HTTP    │
└────────────────────────────────────────────────────────┘
```

两端用的是**同一个 JSON-RPC 2.0 协议**，只是 framing（newline vs HTTP/SSE）和 session 管理不同。所以"协议转换"实际上是：

1. **decode** HTTP 入站请求 → 标准 JSON-RPC message
2. **route** 给某个 stdio child（按工具前缀）
3. **encode** 成 NDJSON 写 child stdin
4. **read** child stdout，把响应 / notifications 串回 HTTP/SSE

#### 库选型

| 库 | 用途 | 推荐度 |
|---|---|---|
| **`github.com/mark3labs/mcp-go`** | 社区主流，client 和 server 双端都成熟，支持 stdio + SSE + Streamable HTTP | ⭐ 主选 |
| `github.com/modelcontextprotocol/go-sdk` | Anthropic + Google 维护的官方 Go SDK；更新但 API 较底层 | 备选（如果对官方背书有要求） |
| 自研 JSON-RPC 2.0 | spec 不复杂，约 300 行 Go 可实现 | 仅在不想引依赖时考虑 |

选 **mcp-go** 的理由：

- `client.NewStdioMCPClient(cmd, args, env)` 一行起一个 stdio child，自动处理 NDJSON framing + JSON-RPC id 配对 + 子进程生命周期
- `server.NewMCPServer(...)` + `server.NewStreamableHTTPServer(...)` 一行起 Streamable HTTP server，自动处理 SSE 升级、session、heartbeat
- 两端 API 对称（同样的 `Tool` / `Resource` / `Prompt` 类型），桥接代码非常自然

**关键认知**：mcp-go 解决的是**两端各自的协议框架**——它不知道"聚合多个 child"这件事。**聚合是 sandrpod 自己的胶水代码**，但工作量很小，因为协议层都已经被 mcp-go 处理掉了。

#### 桥接核心伪代码

```go
// pkg/mcpbridge/child.go
import "github.com/mark3labs/mcp-go/client"

type Child struct {
    Name   string
    Alias  string
    Client *client.Client  // mcp-go stdio client
    tools  []mcp.Tool
}

func (c *Child) Start(ctx context.Context, cfg ServerConfig) error {
    // mcp-go 起 stdio child + 完成 initialize 握手
    cli, err := client.NewStdioMCPClient(
        cfg.Command,
        envSliceFromMap(cfg.Env),
        cfg.Args...,
    )
    if err != nil { return err }
    c.Client = cli

    // initialize handshake
    initReq := mcp.InitializeRequest{...}
    initResp, err := cli.Initialize(ctx, initReq)
    if err != nil { return err }

    // 缓存工具清单
    listResp, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
    if err != nil { return err }
    c.tools = listResp.Tools

    return nil
}
```

```go
// pkg/mcpbridge/aggregator.go
import "github.com/mark3labs/mcp-go/server"

func NewAggregatorServer(mgr *ChildManager) *server.MCPServer {
    s := server.NewMCPServer(
        "sandrpod-mcp-bridge",
        "0.1.0",
        server.WithToolCapabilities(true),
        server.WithResourceCapabilities(true, true),
    )

    // 为每个 child 的每个 tool 在 aggregator 上注册一个 handler
    // 注册时把工具名加前缀: <alias>__<tool_name>
    for _, child := range mgr.children {
        for _, t := range child.tools {
            fqName := child.Alias + "__" + t.Name
            fqTool := mcp.Tool{
                Name:        fqName,
                Description: fmt.Sprintf("[%s] %s", child.Alias, t.Description),
                InputSchema: t.InputSchema,
            }
            s.AddTool(fqTool, makeProxyHandler(child, t.Name))
        }
    }
    return s
}

func makeProxyHandler(child *Child, originalName string) server.ToolHandlerFunc {
    return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
        // 1. 把请求里的工具名换回 child 视角的原始名（去掉前缀）
        req.Params.Name = originalName
        // 2. 透传给 child
        return child.Client.CallTool(ctx, req)
    }
}
```

```go
// pkg/mcpbridge/handler.go
func NewHTTPHandler(aggregator *server.MCPServer) http.Handler {
    // mcp-go 提供 Streamable HTTP transport，自动支持：
    //   - POST /mcp with Accept: application/json     → 单次响应
    //   - POST /mcp with Accept: text/event-stream    → SSE 升级
    //   - GET  /mcp (老式 SSE)                         → 仅向后兼容
    //   - notifications 用 SSE 推送
    //   - session 管理（Mcp-Session-Id 头）
    return server.NewStreamableHTTPServer(aggregator)
}
```

**注**：以上是面向 mcp-go v0.x API 的示意，实际 API 名可能在版本间小变（这个库还在快速演进），落地时按当时版本的 godoc 微调。

#### Streamable HTTP 模式与时机

MCP 2025-03-26 spec 让一个 endpoint 同时支持三种语义，**由客户端 `Accept` 头决定**：

| 客户端 Accept | 服务端行为 | 适合场景 |
|---|---|---|
| `application/json` | 返回单条 JSON-RPC 响应即关闭 | 短工具调用（< 1s）|
| `text/event-stream` | 升级为 SSE，可推 N 条消息后关闭 | 工具有进度通知 / 流式 result |
| 不带 Accept (老 SSE 模式) | 同时开 GET 接收推送 + POST 发请求 | 旧 SDK 兼容 |

mcp-go 的 `StreamableHTTPServer` 自动处理这三种 negotiation。Sandrpod 这边不需要写任何 SSE / chunked encoding 代码——只要保证 stdout reader goroutine 把 child 的 notifications 透传到 mcp-go server 的 `SendNotification` 即可：

```go
// child stdout reader goroutine
for scanner.Scan() {
    line := scanner.Bytes()
    var msg jsonrpc.Message
    json.Unmarshal(line, &msg)

    if msg.IsNotification() {
        // 透传：child 的 notifications/progress → aggregator → 上游 HTTP SSE
        aggregator.SendNotification(msg.Method, msg.Params)
    } else {
        // response，唤醒等待中的 Call() 协程
        c.dispatchResponse(msg)
    }
}
```

#### Capability 合并策略

`initialize` 握手时，bridge 需要告诉上游它支持什么。策略是**所有 child 能力的并集**：

| 能力 | 合并方式 |
|---|---|
| `tools` | 任一 child 支持 → bridge 支持 |
| `resources` | 任一 child 支持 → bridge 支持；list 时合并 |
| `prompts` | 任一 child 支持 → bridge 支持；list 时合并 |
| `sampling` | **永远 false**（v1 不做反向调用，见 §十四 限制） |
| `logging` | 任一 child 支持 → bridge 支持；按 child 维度过滤 |
| `experimental.*` | **不透传**（避免上游误用某个 child 的私有扩展） |

Capability 也是上游观察 child 上下线的信号：某个 child 崩了 → bridge 发 `notifications/tools/list_changed` → 上游重新 ListTools，新清单里就没有崩掉的 child 的工具了。

#### Error 映射

| child 返回 | bridge 透传给上游 |
|---|---|
| JSON-RPC error code `-32601` (method not found) | 同 code 透传 |
| JSON-RPC error code `-32602` (invalid params) | 同 code 透传 |
| `tools/call` result with `isError: true` | 原样透传（这是 MCP 标准的"工具内部错误"，与 transport error 区分）|
| stdio child exit 0 但无响应 | bridge 返回 `-32603 internal error: child returned no response` |
| stdio child crash | bridge 返回 `-32000 server error: child <name> unavailable`，并触发 `tools/list_changed` |
| 超时（默认 30s）| bridge 返回 `-32000 server error: request timeout` |

#### 剩下要 sandrpod 自己写的代码量

mcp-go 解决了协议框架后，sandrpod 自研代码集中在：

| 模块 | 内容 | 代码量估计 |
|---|---|---|
| `child.go` | 起 stdio child + 包装 mcp-go client + 状态机 | ~200 行 |
| `manager.go` | 多 child 调度 + fsnotify 热加载 + 重启策略 | ~300 行 |
| `aggregator.go` | 工具名前缀 + capability 合并 + 路由分发 | ~250 行 |
| `handler.go` | 薄薄包一层 mcp-go server + manifest 端点 | ~100 行 |
| `permission.go` / `audit.go` | 跟 pkg/permission、pkg/audit 的适配器 | ~150 行 |
| 测试 | 单测 + 集成测试（用 mock stdio child）| ~600 行 |

**总计约 1500-1800 行 Go 代码**，是一个紧凑的中型包。mcp-go 大概帮我们省了 2000+ 行协议层代码。

---

## 六、`cmd/agent` 接入

### 新 flag

```go
// cmd/agent/main.go

var (
    // ... existing ...

    mcpConfigPath  = flag.String("mcp-config", envOr("SANDRPOD_MCP_CONFIG", ""),
        "Path to mcp.json (default: ~/.sandrpod/mcp.json; empty disables MCP bridge)")
    mcpEnabled     = flag.Bool("mcp-enabled", envBool("SANDRPOD_MCP_ENABLED", true),
        "Enable MCP transport bridge (default true; set false to disable even if mcp.json exists)")
    mcpReloadOnSig = flag.Bool("mcp-reload-on-hup", true,
        "Reload mcp.json on SIGHUP (in addition to fsnotify)")
)
```

### mux 接入

在 `newAgentMux` 中新增 `/mcp/*` 路由，handler 来自 `pkg/mcpbridge.HTTPHandler`：

```go
// cmd/agent/main.go (新增段)

func newAgentMux(tb http.Handler, sandboxName string, bridge http.Handler) http.Handler {
    mux := http.NewServeMux()
    // ... 现有的 /execute /toolbox/ /process/session/ ...

    if bridge != nil {
        mux.Handle("/mcp", bridge)
        mux.Handle("/mcp/", bridge)
    }
    mux.Handle("/", tb)
    return mux
}
```

### 启动流程

```go
// cmd/agent/main.go func main() 新增段（紧跟 permission/audit 初始化之后）

var bridgeHandler http.Handler
if *mcpEnabled {
    cfgPath := *mcpConfigPath
    if cfgPath == "" {
        cfgPath = defaultMCPConfigPath()  // ~/.sandrpod/mcp.json
    }

    if _, err := os.Stat(cfgPath); err == nil {
        mgr, err := mcpbridge.NewManager(mcpbridge.ManagerOptions{
            ConfigPath: cfgPath,
            Permission: &permissionAdapter{mgr: permMgr},   // 见 §八
            Audit:      &auditAdapter{rec: auditRec},       // 见 §九
            Logger:     log.Default(),
        })
        if err != nil {
            log.Printf("MCP bridge init failed: %v (continuing without bridge)", err)
        } else {
            if err := mgr.Start(ctx); err != nil {
                log.Printf("MCP bridge start failed: %v", err)
            } else {
                bridgeHandler = mcpbridge.NewHTTPHandler(mcpbridge.NewAggregator(mgr))
                log.Printf("MCP bridge ready: %s", cfgPath)
            }
        }
    } else {
        log.Printf("MCP config not found at %s (bridge disabled)", cfgPath)
    }
}

agentHandler := newAgentMux(tbHandler, *name, bridgeHandler)
```

### 优雅关闭

`main()` 收到 SIGTERM/SIGINT 时：

1. agent 主 mux 停止接受新请求
2. `mgr.Stop()` 给所有 child 进程发 SIGTERM，等 5s 后强杀
3. 隧道断开 → API Server 端 sandbox 状态自动转 offline

---

## 七、`cmd/server` 转发路由

### 新路由

```go
// cmd/server/main.go 在 sandbox 路由块新增

// /api/v1/sandboxes/{name}/mcp          → tunnel /mcp
// /api/v1/sandboxes/{name}/mcp/sse      → tunnel /mcp/sse
// /api/v1/sandboxes/{name}/mcp/manifest → tunnel /mcp/manifest

mux.HandleFunc("/api/v1/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
    // ... existing path parsing ...

    if rest, ok := strings.CutPrefix(suffix, "mcp"); ok {
        t := getTunnel(sandboxName)
        if t == nil {
            http.Error(w, "sandbox offline", http.StatusServiceUnavailable)
            return
        }
        target := "http://agent/mcp" + rest  // rest 是 "" / "/sse" / "/manifest" 等
        proxyHTTP(t, r, target, w)
        return
    }
    // ... existing handlers ...
})
```

### SSE/Streaming 支持

现有 `proxyHTTP` 用的是默认 `http.Client.Do()`，需要确认/补一个 streaming 版本以支持 SSE：

- MCP Streamable HTTP transport 在 SSE 模式下需要服务端持续推送
- `proxyHTTP` 当前的 `io.Copy(w, resp.Body)` 已能流式拷贝
- 需要补：`w.(http.Flusher).Flush()` 在每次写入后调用一次，避免本地 buffer 滞留

建议直接抽一个 `proxyHTTPStreaming` 函数复用，或在现有 `proxyHTTP` 内检测 `Content-Type: text/event-stream` 时自动启用 flush。

### 鉴权（已升级两层模型，见 [`MCP_AUTH_HEADER_CONFLICT_FIX.md`](MCP_AUTH_HEADER_CONFLICT_FIX.md)）

最初版本只有一层鉴权（API Server `Authorization: Bearer cfg.Token`），引入 `--mcp-token` 后两个值都要塞 `Authorization`，单 header 装不下。修复后改为两个 header 分担：

| 头 | 谁验证 | 值 |
|---|---|---|
| `X-Sandrpod-Token` | API Server | `cfg.Token`（sandrpod 平台级令牌） |
| `Authorization: Bearer …` | agent 侧 `mcpTokenMiddleware` | `--mcp-token`（sandbox 资源级令牌） |

- API Server **不消费** `Authorization`，原样透传给 agent
- agent 即使收到经隧道的请求，也再用 Bearer 自己验证一次（defense-in-depth）
- API Server 被攻陷只能 replay 截获请求，无法伪造新调用（不持有 mcp-token）
- 老客户端 `Authorization: Bearer cfg.Token` 走兼容路径，仅用于非 `/mcp` 路由

---

## 八、与 `pkg/permission` 集成

### 决策点

| 时机 | 决策类型 | 默认值 |
|---|---|---|
| `mcp.json` 中新增 server 首次启动 | `mcp.install` | `prompt` |
| 启动 server 时执行 command（外部可执行文件） | `mcp.spawn` | `prompt`（command 不在白名单时） |
| 调用某个 tool | `mcp.call.<alias>.<tool>` | `allow`（敏感工具按需 prompt） |
| 子进程崩溃后自动重启 | `mcp.restart` | `allow`（受 `max_restart_per_min` 限制） |

### 复用现有 permission gate

`pkg/permission.Manager` 当前的决策模型（`off` / `prompt` / `strict`）天然适用：

- `--permission-mode=off`：全放行
- `--permission-mode=prompt`：首次安装/启动弹同意对话框
- `--permission-mode=strict`：mcp.json 中未在 `allowed_mcp_servers.json` 显式声明的 server 一律 deny

### 新决策 Source

在 `pkg/audit/event.go` 中增加：

```go
const (
    SourceMCPInstall = Source("mcp.install")
    SourceMCPSpawn   = Source("mcp.spawn")
    SourceMCPCall    = Source("mcp.call")
    SourceMCPRestart = Source("mcp.restart")
)
```

### permission/notify 提示文案

```
SandrPod wants to enable a new MCP server on this machine:

  Name:    github
  Command: npx -y @modelcontextprotocol/server-github
  Env vars: GITHUB_PERSONAL_ACCESS_TOKEN (value not shown)

Allowing means this command will run with your user privileges and
the listed env vars set. Allow?

  [ Allow once ]  [ Allow & remember ]  [ Deny ]
```

`Allow & remember` 写入 `~/.sandrpod/permissions.json` 的 `mcp_servers` 块（与现有 path/command 持久化机制同构）。

---

## 九、与 `pkg/audit` 集成

### 新事件类型

每个 `tools/call` 都产生一条审计事件：

```json
{
  "event_id": "01HXY...",
  "timestamp": "2026-05-27T10:30:42.123Z",
  "source": "mcp.call",
  "decision": "allow",
  "caller": "tunnel://api-server/acme-prod",
  "session_id": "<MCP session id>",
  "matched_command": "github__list_issues",
  "reason": "mcp tool invocation",
  "extras": {
    "mcp_alias": "github",
    "tool_name": "list_issues",
    "args_summary": "{\"repo\":\"acme/x\", \"state\":\"open\"}",
    "result_status": "ok",
    "duration_ms": 412
  }
}
```

### 敏感字段处理

- `args_summary`：truncate 到 1KB；如果检测到长度异常的字符串（>500 字符）替换为 `<redacted:large>`；env 变量名出现在 args 时整段 redact
- **绝不审计 result 内容**：result 可能包含个人邮件/PR/Notion 页面正文，按 GDPR 视角是用户数据，只记 status + size

### 失败的特殊处理

| 失败类型 | decision | reason |
|---|---|---|
| MCP server 启动失败 | `deny` | `spawn failed: <stderr 摘要>` |
| MCP server 崩溃 | `warn` | `crashed: <stderr 摘要>` |
| 工具调用超时 | `warn` | `timeout after 30s` |
| 子工具返回 error | `allow` + `result_status=error` | （调用本身允许，工具内部错） |

### 复用 uploader

直接接 `pkg/audit.Uploader` 现有 batching/HTTP 上报机制，无需新逻辑。

---

## 十、与 `cmd/sandrpod-tray` 集成

Tray 是员工可见的 GUI，承担**安装、状态展示、临时禁用、查看日志**这些 UX 职责。

### 新增 Tray 菜单项

```
SandrPod
├── Status: Running ✓
├── ─────────────────────
├── MCP Servers ▸
│   ├── github        ✓ ready (8 tools)
│   ├── jira          ⚠ failed (click to view error)
│   ├── notion        ✓ ready (5 tools)
│   ├── ─────────────────────
│   ├── Open mcp.json in editor
│   ├── Reload config (Ctrl+R)
│   └── Disable all MCP for 1 hour
├── ─────────────────────
├── Permission gate ▸
└── Quit
```

### 子菜单：单个 MCP server

```
github (ready)
├── Tools: 8
├── Uptime: 12m
├── Restarts: 0
├── ─────────────────────
├── View last error
├── Restart
└── Temporarily disable
```

### Tray 与 agent 通信

复用现有 `pkg/permission/ipc.go` 的 Unix socket / Named Pipe 通道，加一组新方法：

| IPC method | 功能 |
|---|---|
| `mcp.manifest` | 获取当前所有 server 状态（== HTTP `/mcp/manifest`） |
| `mcp.reload` | 重新读取 mcp.json |
| `mcp.restart_server` | 重启指定 server |
| `mcp.disable_server` | 临时禁用（不写回 mcp.json） |
| `mcp.set_global_disabled` | 全局开关 |

Tray 本身不直接读 mcp.json（避免锁竞争），全部走 IPC。

---

## 十一、独立使用模式（无 tunnel）

为支持「我就想本地测试 stdio MCP，不想跑 sandrpod 全套」的开发者场景：

```bash
sandrpod-agent \
  --mcp-only \
  --mcp-config=./mcp.json \
  --listen=127.0.0.1:7090
```

`--mcp-only` 时：

- **不连接 API Server**（跳过 `connectLoop`）
- **不启动 toolbox**（节省内存）
- HTTP 监听本地端口，路径只暴露 `/mcp` 和 `/mcp/manifest`

任意 MCP client 直接连 `http://127.0.0.1:7090/mcp` 即可。这条路径让 sandrpod 在「**本地 MCP 聚合器**」这个独立产品形态下也能用——和 mcp-proxy / supergateway 等社区工具是直接竞品/替代品，但带 SandrPod 的 permission 网关和审计加成。

---

## 十二、消费方对接

### MCP 标准客户端（推荐路径）

任何使用以下 SDK 的 AI 编排器无需特殊适配：

| Stack | Client |
|---|---|
| Python | `mcp` 官方 SDK，`StreamableHttpClientTransport` |
| TypeScript | `@modelcontextprotocol/sdk`，`StreamableHTTPClientTransport` |
| LangChain Python | `langchain-mcp-adapters` |
| OpenAI / Anthropic 直接调用 | 由编排器把 tools 注入 `tools=[...]` 参数 |

连接 URL：

- 经 SandrPod API Server：`https://your-sandrpod.example.com/api/v1/sandboxes/{sandbox_name}/mcp`
- 独立模式：`http://employee-pc.local:7090/mcp`

### Acme 平台对接（参考）

Acme 后端的 `mcp_service_manager` 在加载 session 工具时：

1. 解析 orchestrator config 的 `personal_mcp.enabled`
2. 如启用，通过 `sandbox_user_resolver` 找到当前用户的 sandbox
3. 查询 `/api/v1/sandboxes/{name}/mcp/manifest` 获取在线工具
4. 把这些工具与企业级 `mcp_tools` 表 union 后注入 LangGraph agent
5. 调用时复用现有 MCP client，URL 指向 `https://sandrpod-internal/api/v1/sandboxes/{name}/mcp`

Acme 这边新增的最小工作量：

- `mcp_tools` 表加 `provenance` (`corp` / `personal`) + `sandbox_id` + `owner_user_id` 字段
- 注册接口 `POST /api/agent-system/sandboxes/{id}/mcp/sync`（sandrpod-agent 启动后回调）
- License v2 新 feature key：`mcp.personal`
- DeepAgentsExecutor 工具加载阶段 union 个人 MCP

详细 Acme 侧改动建议放在另一份 `devdocs/PLATFORM_PERSONAL_MCP_INTEGRATION.md`（本文不展开）。

### langchain-sandrpod 集成

`langchain-sandrpod` 包已经把 sandrpod sandbox 暴露为 deepagents backend。加一个 helper：

```python
from langchain_sandrpod import SandrpodBackend
from langchain_mcp_adapters.client import MultiServerMCPClient

backend = SandrpodBackend(api_url=..., sandbox_name="my-laptop")
personal_mcp_url = backend.mcp_url()   # 自动拼出 .../sandboxes/my-laptop/mcp

client = MultiServerMCPClient({
    "personal": {"url": personal_mcp_url, "transport": "streamable_http"},
})
tools = await client.get_tools()
```

---

## 十三、错误处理与离线降级

| 场景 | 系统行为 | 消费方看到 |
|---|---|---|
| 员工 PC 关机 | Tunnel 断开；API Server 把 sandbox 标 offline | `/mcp` → 503 + Retry-After |
| mcp.json 不存在 | bridge 整体禁用 | `/mcp` → 404 |
| mcp.json 解析失败 | bridge 拒绝启动；agent 其余功能正常 | `/mcp/manifest` 暴露 `last_error` |
| 单个 server 启动失败 | 其他 server 正常；该 server 标 `failed` | 该 server 工具不出现在 tools/list；manifest 显示 error |
| 单个 server 运行时崩溃 | 走 restart_policy；超限标 `failed` | 该 server 工具暂时消失；触发 `tools/list_changed` 通知 |
| tools/call 超时 | 默认 30s，触发后 abort 子请求 | MCP 标准 error: `request_timeout` |
| 单个 server 占用 CPU 过高 | 触发本机 cgroup 限制（如有）；agent 不内置限制 | 客户端正常请求，但响应慢 |

### 健康检查

`/mcp/manifest` 是 lightweight 健康检查端点。建议消费方：

- session 启动前 GET 一次 manifest 缓存工具列表
- 长 session 内通过 MCP `notifications/tools/list_changed` 接收变更
- 调用工具失败 → 重新 GET manifest 看 server 状态

---

## 十四、安全模型与已知限制

### 安全模型

| 层 | 威胁 | 缓解 |
|---|---|---|
| mcp.json 文件 | 攻击者改 mcp.json 注入恶意 server | 文件权限 0600 + fsnotify 检测到改动时触发 permission gate 重新同意 |
| 子进程 command | 任意可执行被运行 | `mcp.spawn` 决策点 + `allowed_commands` 白名单（strict 模式）|
| 凭据泄露 | env 变量泄漏到日志 | 审计层硬编码 redact；child 进程 stderr 默认不写盘 |
| 隧道劫持 | API Server 被入侵后调用员工 PC 上的工具 | 复用 SandrPod 现有 sandbox token 鉴权；员工可在 tray 一键停用 |
| 工具 result 数据外泄 | tools/call 返回值经 API Server 流出 | 不审计 result 内容；消费方平台自行加 DLP |

### 已知限制

1. **MCP server 之间无 IPC**：每个 child 独立进程，filesystem MCP 不会自动 expose 给 github MCP。这是设计选择（隔离 > 协作）
2. **不支持 MCP `sampling`（反向 LLM 调用）**：v1 仅做单向 tool invocation；sampling 需要 child → agg → 隧道 → API Server → 消费方 LLM 的反向通路，复杂度大幅上升，留作 v2
3. **没有跨 sandbox MCP 共享**：员工 A 的 github MCP 不会自动给员工 B 用——这是隐私优势但有时是产品需求；将来可加「共享 MCP server」概念，跑在 poder/docker 上
4. **mcp.json 修改即时生效**：fsnotify 触发热加载，可能导致正在执行的 tools/call 被中断。建议消费方按 `tools/list_changed` 重试

---

## 十五、实施路径

### 阶段 A：MVP（2 周，单人）

| Day | 工作项 |
|---|---|
| D1–D2 | `pkg/mcpbridge/config.go` + `child.go`（stdio JSON-RPC client，initialize + tools/list + tools/call）|
| D3–D4 | `manager.go`（多 child 调度）+ `aggregator.go`（命名空间合并）|
| D5–D6 | `handler.go`（Streamable HTTP server，先实现 plain POST，SSE 留到 B 阶段）|
| D7 | `cmd/agent` 接入新 flag + mux 路由；本地端到端跑通一个 npx github MCP |
| D8 | `cmd/server` 加 `/api/v1/sandboxes/{name}/mcp` 转发；远端连通性测试 |
| D9 | `/mcp/manifest` 端点；基础健康检查 |
| D10 | 单测（child mock、manager hot-reload、aggregator namespace）|

**交付物**：一个员工电脑装好后，远端 curl `POST /api/v1/sandboxes/.../mcp` 调 `tools/list` 能拿到他本机 github MCP 的工具清单，调 `tools/call` 能执行。

### 阶段 B：生产化（2 周）

| Day | 工作项 |
|---|---|
| D1–D2 | SSE 模式 + `proxyHTTPStreaming` 在 server 端补 flush |
| D3 | restart_policy + 崩溃恢复 + `max_restart_per_min` 限流 |
| D4 | fsnotify 热加载 mcp.json + `tools/list_changed` 通知 |
| D5–D6 | `pkg/permission` 集成（`mcp.install` / `mcp.spawn` 决策）+ `~/.sandrpod/permissions.json` 持久化 |
| D7–D8 | `pkg/audit` 集成（`mcp.call` 事件 + redact 规则）+ 单测 |
| D9 | `--mcp-only` 独立模式 + 文档 |
| D10 | 集成测试 + 压测（10 个 server 并发 100 tools/call/s）|

### 阶段 C：UX 与生态（1-2 周）

| Day | 工作项 |
|---|---|
| D1–D3 | `cmd/sandrpod-tray` 新菜单 + IPC method |
| D4 | langchain-sandrpod 加 `backend.mcp_url()` helper + 示例 |
| D5–D6 | 文档：`docs/MCP_BRIDGE.md`（含 Claude Desktop 配置迁移指南、消费方对接说明）|
| D7+ | 早期用户 dogfood，根据反馈微调 |

### 总工作量

**约 5-6 周单人工时**，比之前估的 4-6 周略多，但**所有不确定性已经在设计阶段消除**：

- 复用 MCP 标准协议（spec 已稳定）
- 复用 SandrPod 现有 tunnel / permission / audit / tray 基础设施
- 不发明新协议、不引入新依赖
- 子进程管理是成熟工程问题

---

## 附录 A：与 mcp-proxy / supergateway 的差异

| 维度 | mcp-proxy | supergateway | **SandrPod MCP Bridge** |
|---|---|---|---|
| stdio → HTTP | ✅ 单 server | ✅ named server 模式可多 server | ✅ 多 server 自动聚合 |
| 工具命名空间合并 | ❌ 每个 server 独立 endpoint | ⚠ 路径分隔，不合并 tools | ✅ 单 endpoint + 前缀合并 |
| 跨网络（远端访问员工 PC） | ❌（仅本机） | ❌（仅本机） | ✅ 复用反向隧道 |
| 凭据本地化 | ✅ | ✅ | ✅ |
| 权限同意网关 | ❌ | ❌ | ✅（复用 pkg/permission） |
| 审计 | ❌ | ❌ | ✅（复用 pkg/audit） |
| 进程崩溃恢复 | ⚠ 基本 | ⚠ 基本 | ✅ 策略化 + tray 可见 |
| GUI 管理 | ❌ | ❌ | ✅ sandrpod-tray |

SandrPod MCP Bridge 不试图替代 mcp-proxy/supergateway——它**包含**它们的能力，并叠加了远程访问 + 权限 + 审计这些企业级需求。本地纯开发场景下，独立模式（§十一）已经能直接替代这两个工具。

---

## 附录 B：mcp.json 安全迁移建议

员工从 Claude Desktop 迁移配置时：

1. 备份：`cp ~/Library/Application\ Support/Claude/claude_desktop_config.json ~/.claude_desktop_backup.json`
2. 复制：`cp ~/Library/Application\ Support/Claude/claude_desktop_config.json ~/.sandrpod/mcp.json`
3. 收紧权限：`chmod 600 ~/.sandrpod/mcp.json`
4. 检查 env 块：把组织敏感凭据 rotate 一次（已有 token 仍有效，但应有「这个 token 现在可能被另一个工具访问」的意识）
5. tray 启动后会显示新 server 列表，逐个 `Allow & remember`

文档应明确说明：**Claude Desktop 和 SandrPod 同时启用同一份 mcp.json 时，两个进程会各自 fork 一份 stdio child**——某些 MCP server 持有独占资源（如 SQLite 文件锁）会冲突。建议二选一，或为 SandrPod 维护一份独立 mcp.json。

---

*Last Updated: 2026-05-27*
*Maintainers: SandrPod 维护者 + Acme 平台团队（消费方代表）*
