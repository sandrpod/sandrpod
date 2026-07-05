# E2B MCP Gateway 兼容

本文说明 sandrpod 如何兼容 E2B 的 **in-sandbox MCP Gateway**：未改动的 E2B SDK
（`Sandbox.create({ mcp })` → `getMcpUrl()` / `getMcpToken()`）连到 sandrpod 时，
MCP 工具能像连 E2B 一样即插即用。同时诚实标注和 E2B 的**唯一实质差异**。

## 一句话结论

**原理相同，客户端契约完全兼容，工具启动机制不同。**

- ✅ 传输面（URL / token / Bearer / Streamable-HTTP）：**drop-in 兼容**。
- ✅ 自定义 server 配置（`{installCmd, runCmd}`）：**完全支持**。
- ⚠️ Docker MCP Catalog 配置（文档默认写法 `{exa:{apiKey}}`）：**按精选清单兼容**，
  不是全部 200+；根因是 E2B 把每个工具**当 Docker 容器**跑，sandrpod 把它当
  **stdio 子进程**（npx/uvx）跑 —— 工具本身等价，启动方式不同。

## 和现有 sandrpod MCP 的关系（两个 surface，同一个引擎）

> 两个高频问题：现有 MCP 还能用吗？为什么不复用 mcpbridge、非要加个 `mcp-gateway`？

**现有 sandrpod MCP 完全没动，照常用。** 本特性是纯新增，未改 `pkg/mcpbridge`，
也未改 toolbox 自带的 `/mcp`。沙箱里现在有两个**互相独立**的 MCP surface：

| | sandrpod 原生 MCP | E2B 兼容 MCP（本特性） |
|---|---|---|
| 挂载 | toolbox `:8080/mcp` | mcp-gateway `:50005/mcp` |
| 对外 | `/api/v1/sandboxes/{name}/toolbox/mcp`（或 agent `action=mcp`） | `50005-<id>.<domain>/mcp` |
| 配置文件 | `/workspace/.sandrpod/mcp.json`（`DefaultConfigPath()`，register-mcp skill 热改） | `/etc/mcp-gateway/config.json`（`-state-dir`） |
| 鉴权 | `SANDRPOD_MCP_TOKEN`（可选） | `GATEWAY_ACCESS_TOKEN`（Bearer，E2B 契约） |
| 何时起 | 容器启动即起（默认 `-mcp-enabled`） | 仅当有人跑 `mcp-gateway` 时（E2B `create({mcp})` 触发） |

配置文件**故意分开**，两者互不覆盖。

**`cmd/mcp-gateway` 其实就是复用 mcpbridge。** 它不是另写一套 MCP，而是
`pkg/mcpbridge` 的一个 ~140 行薄壳，核心两行和 toolbox 里一模一样：

```go
mgr := mcpbridge.NewManager(...)          // 同一个引擎
handler := mcpbridge.NewHTTPHandler(mgr)   // 同一个引擎
```

即：`pkg/mcpbridge` 是**被复用的引擎**；`cmd/toolbox` 与 `cmd/mcp-gateway` 是两个薄入口。

**那为什么还要一个独立进程/二进制，而不是让 toolbox 自带 bridge 顶上？** 因为
E2B SDK 的**运行时契约**是 toolbox 那个"开机就起"的 bridge 满足不了的：

1. E2B SDK 会在沙箱里真的执行 `mcp-gateway --config '...'` → PATH 上必须有个叫
   `mcp-gateway` 的可执行文件，否则 `create({mcp})` 直接 command not found。
2. 端口写死 `:50005`（`getMcpUrl()` 拼 `50005-<id>`）；toolbox bridge 在 `:8080`。
3. token 是客户端每次 create 现生成的（`crypto.randomUUID`），在**启动那一刻**经
   `GATEWAY_ACCESS_TOKEN` env 传入；开机就起的 bridge 拿不到这个 per-sandbox token。
4. token 要落到 `/etc/mcp-gateway/.token`（`getMcpToken()` 从这读）。
5. 配置隔离：E2B 传入的 mcp 配置不该覆盖 in-sandbox agent 正在编辑的 `mcp.json`。

要让 toolbox 自带 bridge 去顶这套，反而更复杂（得把 per-spawn 的 token+配置经 IPC
交给开机那个 bridge，还把 E2B 端口/token 味道塞进核心 toolbox）。薄壳进程更干净，
且**按需启动 —— 不用 E2B 的 MCP 就永不启动、零开销**。

## E2B 的原理

E2B 在沙箱内运行一个 **MCP Gateway**（监听 `:50005`），把 N 个后端 MCP server
聚合成一个标准 **MCP Streamable-HTTP** 端点 `/mcp`，用 `GATEWAY_ACCESS_TOKEN`
做 Bearer 鉴权。SDK 侧：

```ts
const sbx = await Sandbox.create({ mcp: { exa: { apiKey: "…" } } })
const url   = sbx.getMcpUrl()        // https://50005-<id>.<domain>/mcp（纯客户端拼接）
const token = await sbx.getMcpToken()// files.read('/etc/mcp-gateway/.token')
// 客户端连接：Authorization: Bearer <token>
```

E2B 的 gateway 用 **Docker MCP Catalog** 把 `exa`/`github`/`airtable` 等名字解析成
**Docker 镜像**，每个工具作为一个独立容器运行，凭据注入为容器环境变量
（“Each MCP tool runs as a Docker container inside the E2B sandbox”）。

## sandrpod 的对应实现

sandrpod 的 [`pkg/mcpbridge`](../pkg/mcpbridge) 早已是**同一个角色**：把
`mcpServers` 里的 N 个 stdio/remote MCP server 聚合成一个 Streamable-HTTP `/mcp`。
为兼容 E2B，新增两块：

### 1. `cmd/mcp-gateway` —— E2B `mcp-gateway` 二进制的 drop-in

装进 toolbox 镜像的 `/usr/local/bin/mcp-gateway`。E2B SDK 在沙箱内执行
`mcp-gateway --config '<json>'`（env `GATEWAY_ACCESS_TOKEN=<token>`）时，本 shim：

1. 把 `--config` 翻译成 mcpbridge 配置（三种输入形状，见下）。
2. 用 `pkg/mcpbridge` 在 `:50005/mcp` 上聚合并对外服务。
3. Bearer 校验 `GATEWAY_ACCESS_TOKEN`（constant-time）。
4. 把 token 写到 `/etc/mcp-gateway/.token`，供 `getMcpToken()` 读取。

配置文件独立于 toolbox 自带 bridge（写 `/etc/mcp-gateway/config.json`，**不碰**
`/workspace/.sandrpod/mcp.json`），两个 bridge 互不干扰。

### 2. 服务端通用端口路由（`pkg/e2bcompat` + `cmd/server`）

E2B host 路由新增“通用端口”分支：`<port>-<id>.<domain>/<path>`，当 port 既不是
envd 也不是 code interpreter 端口、path 也不是 envd/code 路径时，经隧道代理到该沙箱
toolbox 的 `/proxy/<port>/` 挂载点 → 沙箱内 `127.0.0.1:<port>`。于是
`https://50005-<id>.<domain>/mcp` 就能打到沙箱里的 mcp-gateway。

- 用**流式**代理（`proxyHTTPStreaming`），保证 MCP Streamable-HTTP 的 SSE 实时 flush。
- 多实例 LOAD 模式下，先走 `Forwarder`（`X-Sandrpod-Forwarded` 防环）转发到隧道
  所在节点，与 envd/code 面路由一致。
- 钩子：`e2bcompat.Config.PortProxy`，服务端实现 `e2bDeps.portProxy`。

## `--config` 支持的三种形状

`translateConfig`（`cmd/mcp-gateway/config.go`）接受：

| 形状 | 例子 | 处理 |
|------|------|------|
| sandrpod 原生 | `{"mcpServers":{"fs":{"command":"npx",...}}}` | 原样使用 |
| E2B 自定义 server | `{"weather":{"installCmd":"pip install x","runCmd":"python -m s"}}` | `sh -c "install && run"` 起 stdio 子进程；key 形如 `owner/repo` 时先 `git clone` |
| E2B Docker Catalog | `{"exa":{"apiKey":"…"}}` | 查精选清单 → npx/uvx 子进程 + 凭据注入 env |

### Catalog 精选清单（`cmd/mcp-gateway/catalog.go`）

不是全 200+，而是把最常用的 Catalog server 名映射到**真实存在的 npm 包** + 凭据
env 名（凭据 env 名取自 E2B 文档示例与 Docker MCP Registry 的 `server.yaml`）：

| Catalog 名 | 启动 | 凭据 → env |
|-----------|------|-----------|
| `exa` | `npx -y exa-mcp-server` | `apiKey`→`EXA_API_KEY` |
| `brave` / `brave-search` | `npx -y @modelcontextprotocol/server-brave-search` | `apiKey`→`BRAVE_API_KEY` |
| `github` / `github-official` | `npx -y @modelcontextprotocol/server-github` | `personalAccessToken`/`token`/…→`GITHUB_PERSONAL_ACCESS_TOKEN` |
| `airtable` | `npx -y airtable-mcp-server` | `airtableApiKey`→`AIRTABLE_API_KEY` |
| `browserbase` | `npx -y @browserbasehq/mcp-server-browserbase` | `apiKey`→`BROWSERBASE_API_KEY`, `projectId`→`BROWSERBASE_PROJECT_ID`, `geminiApiKey`→`GEMINI_API_KEY` |
| `slack` | `npx -y @modelcontextprotocol/server-slack` | `botToken`→`SLACK_BOT_TOKEN`, `teamId`→`SLACK_TEAM_ID` |
| `filesystem` | `npx -y @modelcontextprotocol/server-filesystem /workspace` | — |

- 未在映射里的凭据 key → 兜底 `UPPER_SNAKE_CASE`（例如 `geminiApiKey`→`GEMINI_API_KEY`）。
- **不在清单里的 Catalog 名** → 跳过并打 warning，提示改用 `{installCmd, runCmd}` 显式形状
  （Catalog 镜像名无可猜的规律：`brave`→`mcp/brave-search`，`github-official`→`ghcr.io/…`）。

## 和 E2B 的实质差异（一条）

E2B：Catalog 工具 = **Docker 容器**（沙箱内需要 Docker）。
sandrpod poder 沙箱本身是容器、内部无 Docker，故 Catalog 工具跑成 **stdio 子进程**
（npx/uvx，toolbox 镜像已内置 node/npm + uv/uvx）。

影响：
- 工具**行为等价**（同样的 MCP server 实现），对 agent 透明。
- 覆盖面：精选清单内即插即用；清单外用显式 `runCmd`（万能，无需 Docker）。
- 若要 100% 复刻“每工具一容器”，需要沙箱内 DinD —— 目前不做，因为 stdio 路径已覆盖
  绝大多数场景且更轻。

## 适用范围：仅 poder（容器）沙箱，agent 沙箱不自动支持

E2B MCP gateway 只对 **poder(Docker 容器)沙箱**开箱可用，因为 `mcp-gateway` 二进制
只 COPY 进了 **toolbox 镜像**。**agent(`direct://`)沙箱**——即 `sandrpod-agent` 把
本机注册为沙箱——**不自动支持**：

- 路由那半边是通的：server 侧 `portProxy` 对 `direct://` 分支代理到 `http://agent/proxy/<port>/`，
  agent mux 末尾 `mux.Handle("/", tb)` 兜底转给内嵌 toolbox 的 `/proxy/{port}/` → `127.0.0.1:<port>`。
- 但 `sandrpod-agent` 是独立二进制、不含 `mcp-gateway`,故 `create({mcp})` 在 agent
  机器上 `commands.run('mcp-gateway …')` = command not found,:50005 无人监听。
- 这对**员工机模式**反而更安全:自动在员工电脑上拉起 MCP 工具与 permission gate 的
  初衷相悖,command-not-found 是 fail-closed。
- 若确需让某台 agent 机器支持:自行把 `mcp-gateway` 放到该机 PATH 并在 :50005 起服务
  (employee 模式下仍受 permission gate 约束)。默认不做。

## 端到端流程（drop-in）

1. `Sandbox.create({ mcp })` → SDK 在沙箱内 `mcp-gateway --config …`（env token）
   → shim 起 `:50005/mcp`，写 `/etc/mcp-gateway/.token`。
2. `getMcpUrl()` → `https://50005-<id>.<domain>/mcp` → 服务端通用端口路由 → 隧道 →
   toolbox `/proxy/50005/mcp` → 沙箱内 `127.0.0.1:50005`（shim）。
3. `getMcpToken()` → `files.read('/etc/mcp-gateway/.token')` → envd → shim 写的 token。
4. MCP 客户端带 `Authorization: Bearer <token>` 连上 → shim 校验 → mcpbridge 聚合服务。

## 相关代码

- `cmd/mcp-gateway/` —— shim（`main.go` / `config.go` / `catalog.go` + 测试）
- `pkg/e2bcompat/gateway.go` —— `Config.PortProxy` 钩子 + 通用端口分发
- `cmd/server/e2bgateway.go` —— `e2bDeps.portProxy`（隧道流式代理到 `/proxy/<port>/`）
- `docker/Dockerfile.toolbox` —— 把 `mcp-gateway` 装到 `/usr/local/bin`
- `pkg/mcpbridge/` —— 底层 MCP 聚合器（envd/code 端口 49983/49999，E2B 网关端口 50005）
