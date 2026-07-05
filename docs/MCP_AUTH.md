# MCP Bridge 鉴权

每个 sandrpod 沙箱的 toolbox 里常驻一个 **MCP bridge**:把 `mcp.json` 里的 N 个
stdio/remote MCP server 聚合成一个 `/mcp`(Streamable-HTTP)端点。本文讲**怎么保护
这个端点** —— 通用产品视角,不假设任何上层平台的实现。

## 两层鉴权(两个 header)

对外的 MCP 端点是 `<api-server>/api/v1/sandboxes/{name}/mcp`,经反向隧道到沙箱。它有
**两层可独立开关**的鉴权:

| 层 | header | 谁校验 | 作用 |
|---|---|---|---|
| **平台层** | `X-Sandrpod-Token: <平台 token>` | API Server 的 authMiddleware | "能不能够到这个沙箱"。首选头;`Authorization: Bearer <平台 token>` 作老服务端 legacy 兜底 |
| **资源层(可选)** | `Authorization: Bearer <mcp_token>` | 沙箱内 bridge(`mcpbridge.TokenMiddleware`) | "能不能调这台机器上的 MCP 工具"。API Server **不消费、原样透传**进隧道 |

**为什么两个 header**:一个 `Authorization` 塞不下两个 secret。设计上把 `Authorization`
让给 MCP 资源层(通用 MCP 客户端天然把端点凭据放这里),平台 token 改走
`X-Sandrpod-Token`。历史见 [MCP_AUTH_HEADER_CONFLICT_FIX.md](MCP_AUTH_HEADER_CONFLICT_FIX.md)。

## 资源层(`mcp_token`)是可选的——按租户模型选

`mcp_token` 是 **bridge 的共享密钥**,与平台鉴权解耦、可独立轮换。它防的是一个具体威胁:
**平台运营者 ≠ MCP 工具的主人**时,光有平台 admin 不该等于能触发你机器上带本地凭据的
MCP 工具(github / notion / 文件系统…)。

| 部署形态 | 建议 |
|---|---|
| **单租户 / 自托管**(你同时跑 server 和沙箱/agent,单一信任域) | **不设** `mcp_token`。隧道 + 平台鉴权已是边界,第二层是纯摩擦 |
| **多租户 / 员工机 / BYO-device**(平台方 ≠ 工具主人) | **设** `mcp_token`,每沙箱一个、独立轮换。API Server 即便被攻破也只能重放、无法伪造新调用 |

怎么开(通用,与任何上层 provisioning 无关):

```bash
# 沙箱内(容器 toolbox 或裸机 agent)以共享密钥启动 bridge
sandrpod-agent  -mcp-token <secret> ...       # 或 SANDRPOD_MCP_TOKEN=<secret>
# toolbox 镜像同理:-mcp-token / SANDRPOD_MCP_TOKEN
```

不设时 bridge 会打 WARNING:*"any caller that reaches /mcp can invoke tools"* —— 这是
**有意的**单租户默认(fail-open,靠隧道兜底),不是 bug。

## manifest 只需平台鉴权(默认放行)

`GET /mcp/manifest` 是**只读元数据**(server 名、状态、工具数 —— **不含任何凭据**)。它
**默认豁免** `mcp_token`:过了平台鉴权就能"看有哪些工具",但**调用**工具(`POST /mcp`)
仍需资源层密钥。这符合最小权限,也让 `sandrpod-cli mcp tools` 之类的元数据查询不必持有
每沙箱密钥。

想更严(连"有哪些工具"都算敏感)——把 manifest 也纳入密钥守卫:

```bash
sandrpod-agent -mcp-token <secret> -mcp-guard-manifest ...   # 或 SANDRPOD_MCP_GUARD_MANIFEST=true
```

实现:`pkg/mcpbridge/TokenMiddleware(token, guardManifest, next)`(agent 与 toolbox 共用一份)。

## 调用方怎么带这两个 token

**sandrpod-cli**(平台 token 从配置/`SANDRPOD_API_TOKEN`;个人 token 单独给):
```bash
sandrpod-cli mcp tools <sandbox> --mcp-token <mcp_token>
# 或： export SANDRPOD_MCP_TOKEN=<mcp_token>
```

**langchain-sandrpod**:
```python
sb = SandrPodSandbox(sandbox_name="…", api_token="<平台>", mcp_token="<个人>")
# mcp_token 也可从 SANDRPOD_MCP_TOKEN 读；sb.mcp_tools()/mcp_manifest() 自动带上第二个头
```

**任意 MCP 客户端 / 裸 curl**:
```bash
curl <api-server>/api/v1/sandboxes/<name>/mcp/manifest \
  -H "X-Sandrpod-Token: <平台 token>" \
  -H "Authorization: Bearer <mcp_token>"        # manifest 默认可省;调用 /mcp 时必带
```

## 相关

- [MCP_BRIDGE.md](MCP_BRIDGE.md) — bridge 本身(mcp.json、聚合、热重载)
- [MCP_AUTH_HEADER_CONFLICT_FIX.md](MCP_AUTH_HEADER_CONFLICT_FIX.md) — 两 header 方案的由来
- [AUTH_AND_KEYS.md](AUTH_AND_KEYS.md) — 平台 token 的签发/管理
- 代码:`pkg/mcpbridge/auth.go`(`TokenMiddleware`)、`cmd/agent`/`cmd/toolbox`(`-mcp-token` / `-mcp-guard-manifest`)
