# MCP：让你的工具进入沙箱

> SandrPod 的每个沙箱都内置一个 **MCP bridge**：把你在 `mcp.json` 里配置的
> MCP 服务器（GitHub、Notion、Linear、文件系统……）聚合成一个
> Streamable-HTTP 端点。任何 MCP 客户端都能用，你的凭证**只存在沙箱里**。

## 它解决什么问题

个人智能体（Muse、Claude、自建 agent）需要操作**你的**知识库、**你的**
代码库、**你的**文件——但把凭证交给平台托管，信任成本太高。

SandrPod 的做法：凭证留在你自己的沙箱里，平台只负责把工具调用转发进来。

| | 传统 MCP 托管 | SandrPod 沙箱 MCP |
|---|---|---|
| 凭证在哪 | 平台数据库 | **沙箱内**（0600 文件） |
| 平台能否看到 token | 能 | **不能**（只见一次性授权码） |
| 配置格式 | 各家不同 | Claude Desktop 原格式 |
| 支持的工具面 | 单一服务 | N 个服务聚合成一个端点 |

## 快速开始（3 分钟）

### 1. 添加 MCP 服务器

```bash
# 标准 OAuth 服务（Notion / Linear / GitHub / Sentry）
sandrpod-cli mcp add my-sandbox notion \
  --url https://mcp.notion.com/mcp

# 本地 stdio 服务器（Claude Desktop 配置原样可用）
sandrpod-cli mcp add my-sandbox github \
  --command npx -- -y @modelcontextprotocol/server-github \
  --env GITHUB_PERSONAL_ACCESS_TOKEN=ghp_…
```

### 2. 授权（仅 OAuth 服务需要）

```bash
sandrpod-cli mcp ls my-sandbox
#   notion  →  https://mcp.notion.com/mcp  [waiting_auth]
#     ↳ 授权: https://mcp.notion.com/authorize?client_id=…
```

打开授权链接，在浏览器里点「允许」。回调会**直接进入沙箱**（经 preview
URL 路由），bridge 完成 token 交换，状态变成 `ready`。

控制台里也可以：沙箱详情 → **MCP** 标签页 → 点「授权」按钮。

### 3. 使用

```bash
sandrpod-cli mcp tools my-sandbox        # 列出全部工具
sandrpod-cli mcp url my-sandbox          # 打印对外端点 + 认证头
```

## 对外端点

外部 MCP 客户端（Claude Desktop、Cursor、任何 AI）有两个入口：

### 隧道端点（带认证，推荐）

```
https://cloud.sandrpod.com/api/v1/sandboxes/{name}/mcp
```

```
X-Sandrpod-Token: <你的 API Key>          # 控制台 API Key 页创建
Authorization: Bearer <mcp_token>          # 可选，见下方安全模型
```

### Preview URL（无 header，凭主机名）

```
https://8080-{sandbox_name}.sandbox.sandrpod.com/mcp
```

主机名即能力凭证（与 E2B 一致）。适合不方便带自定义 header 的客户端；
跑敏感工具的沙箱建议用隧道端点。

客户端配置示例（Cursor / Claude Desktop）：

```json
{
  "mcpServers": {
    "my-tools": {
      "url": "https://8080-my-sandbox.sandbox.sandrpod.com/mcp",
      "transport": "streamable_http"
    }
  }
}
```

## 支持的配置

`mcp.json` 与 Claude Desktop / Cursor / Cline **字节级兼容**——现有
`mcpServers` 块原样可用。SandrPod 扩展放在 `sandrpod` 子对象里：

| 字段 | 默认 | 说明 |
|---|---|---|
| `alias` | 键名 | 工具前缀：`<alias>__<tool_name>` |
| `restart_policy` | `always` | `always` / `on-failure` / `never` |
| `max_restart_per_min` | 3 | 超限后标记 `failed` |
| `startup_timeout_sec` | 30 | OAuth 首次授权建议 180 |
| `tool_allowlist` / `tool_denylist` | 全部 / 无 | 工具过滤 |
| `resource_*` / `prompt_*` | 同上 | 资源与 prompt 独立过滤 |

三种服务器类型：

| 类型 | 配置 | 说明 |
|---|---|---|
| stdio | `command` + `args` + `env` | 本地子进程（npx/uvx） |
| Streamable-HTTP | `url` | 远程 MCP（Notion 等） |
| OAuth 远程 | `url` + `auth: "oauth"` | 自动走浏览器授权 |

## OAuth 的工作方式（凭证不出沙箱）

```text
1. bridge 发现 OAuth-only 服务器 → 生成授权 URL（PKCE + 动态客户端注册）
   redirect_uri = https://7099-{sandbox}.{domain}/callback
2. 你在浏览器里点「允许」
3. 提供方重定向到 preview URL → Caddy → 控制面 → 隧道 → 沙箱
4. bridge 拿到一次性 code，**直接向提供方**换 token（出网）
5. token 存沙箱内 /workspace/.sandrpod/oauth/（0600），自动刷新
```

**控制面全程只见一次性授权码，看不到 token。** 吊销 = 删沙箱，或去
提供方页面撤销授权。

## 安全模型

| 层 | 保护什么 | 配置 |
|---|---|---|
| 平台层 `X-Sandrpod-Token` | 能否**到达**沙箱 | API Key（控制台签发） |
| 资源层 `Authorization: Bearer` | 能否**调用**工具 | worker 的 `-mcp-token`（可选） |
| 权限门（agent 模式） | 每次调用是否放行 | `-permission-mode=prompt` |
| 审计 | 谁在什么时候调了什么 | 自动记录，可外送 |

多租户 / 员工 PC 部署**务必设置** `-mcp-token`：平台管理员不应能触发
别人机器上的工具。

## E2B 兼容

`Sandbox.create({ mcp })` + `getMcpUrl()` / `getMcpToken()` 的 E2B SDK
代码原样可用。详见 [E2B 直接替换](/zh/e2b)。

## 常见问题

**OAuth 授权后状态还是 waiting_auth？**
刷新页面或等 30 秒（控制台自动轮询）。如果一直不变，`mcp ls` 看错误信息。

**mcp.json 改了没生效？**
bridge 每 5 秒 watch 一次配置文件。确认路径：`sandrpod-cli mcp ls` 的第一行。

**Notion 授权页面打不开？**
确认沙箱所在的 worker 配置了 `SANDRPOD_E2B_DOMAIN`（preview URL 依赖它）。

**凭证存在哪？怎么删？**
`/workspace/.sandrpod/oauth/`（0600）。删沙箱即删除；或去提供方页面撤销授权。
