# MCP 统一方案：OAuth 回调直达沙箱 + 浏览器能力

> 目标：让**所有类型的沙箱**（agent、容器、E2B 兼容）都能完成 OAuth 授权、
> 运行浏览器自动化，并且**凭证只存在于沙箱内**，不经过控制面。

## 现状与差距

| 能力 | agent（员工 PC） | 容器沙箱 | 差距 |
|---|---|---|---|
| MCP stdio 服务器 | ✅ | ✅ | — |
| MCP HTTP 远程 | ✅ | ✅ | — |
| MCP OAuth | ✅（本机浏览器） | ❌（回环不可达） | **核心缺口** |
| 浏览器自动化 | ✅（本机浏览器） | ❌（无 Chromium） | 产品缺口 |
| 出网白名单 | ✅ | ✅ | — |
| 凭证存储位置 | 沙箱本地 | — | 统一后：沙箱本地 |

核心矛盾：OAuth 需要浏览器 + 回调，容器沙箱两者都没有。

## 方案

### 1. OAuth 回调直达沙箱（利用已有的 preview URL 基础设施）

bridge 的 redirect_uri 从回环地址改为 preview URL：

```
改前：http://127.0.0.1:7099/callback
改后：https://7099-{sandbox_name}.{e2b_domain}/callback
```

已有基础设施（全部验证过）：
- DNS wildcard `*.sandbox.sandrpod.com` → 服务器 IP ✓
- Caddy wildcard + on_demand TLS ✓
- E2B gateway EnvdHostPattern + PortProxy ✓
- toolbox `/proxy/{port}/` 反代 ✓

bridge 在 OAuth 提供方重定向后，直接向提供方换 token（出网），token 存沙箱本地。
控制面在这条链路里只是转发器——**看不到 token**。

### 2. 浏览器能力（Playwright + Chromium in toolbox image）

toolbox 镜像加入 Playwright + Chromium，沙箱内代码可直接做浏览器自动化。
出网白名单照常覆盖浏览器流量。

### 3. 前端：MCP 服务器管理

节点页新增"MCP 服务器"卡片：列出沙箱里的 MCP 服务器、状态、工具计数，
以及 OAuth `waiting_auth` 时的授权按钮。

---

## 实施计划

### Phase 1: OAuth 回调直达沙箱

| 步骤 | 文件 | 改动 |
|---|---|---|
| bridge 接受公开回调地址 | `pkg/mcpbridge/oauth.go` | `redirect_uri` 从回环改为可配置的公开 URL |
| bridge 获取沙箱名 + E2B 域名 | `cmd/poder/enrol.go` | 启动时传入 `SANDRPOD_E2B_DOMAIN` + 沙箱名 |
| 控制面转发路由 | 已有（E2B gateway PortProxy） | 不需要新代码 |
| 测试 | `pkg/mcpbridge/oauth_test.go` | 回调到 preview URL 的端到端测试 |

### Phase 2: 浏览器能力

| 步骤 | 文件 | 改动 |
|---|---|---|
| toolbox 镜像加 Playwright | `docker/Dockerfile.toolbox` | 安装 Chromium + Playwright |
| mcp.go 暴露 browser 工具 | `pkg/toolbox/` | 可选：注册 browser_* 工具集 |

### Phase 3: 控制台 UI

| 步骤 | 文件 | 改动 |
|---|---|---|
| MCP 服务器管理卡片 | `web/src/pages/Workers.tsx` | 列出沙箱内的 MCP 服务器、状态、授权按钮 |

---

## 安全模型

| 保障 | 怎么做到的 |
|---|---|
| token 不经过控制面 | code → token 的交换是沙箱直接向 OAuth 提供方发起的出网请求 |
| 控制面只转发回调 | 转发的是一次性 code（10 分钟过期），不是 token |
| 每个沙箱的 token 互相隔离 | token 存在各自沙箱的文件系统里 |
| 跨沙箱不可见 | 沙箱之间的文件系统互相不可见（容器隔离） |
| 出网白名单覆盖 | 浏览器流量也走代理，白名单照常限制 |
| OS 沙箱覆盖 | Playwright 进程也被 seatbelt/bwrap 限制 |

---

## 已验证的基础设施

| 层 | 状态 |
|---|---|
| DNS wildcard `*.sandbox.sandrpod.com` → 服务器 IP | ✅ |
| Caddy wildcard + on_demand TLS | ✅ |
| E2B gateway EnvdHostPattern + PortProxy | ✅ |
| toolbox `/proxy/{port}/` 反代 | ✅ |
| bridge OAuth 2.1 + PKCE + 动态客户端注册 | ✅ |
| bridge token 自动刷新 | ✅ |

---

## 与 mcp-remote 的关系

mcp-remote 在标准 OAuth 场景下可以退役。保留价值：非标准提供方的兼容层。
移除时机：用户确认新方案稳定后，从文档中降级为"备选方案"。
