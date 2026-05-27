# MCP 路由鉴权头冲突修复方案

> **状态**: 修复设计稿 v0.1（待实施）
> **更新日期**: 2026-05-28
> **严重程度**: 🟥 **Blocker** — 在 `cfg.Token != ""` 的生产部署下，远端 MCP 客户端**无法**同时通过 API Server 鉴权和 agent `--mcp-token` 鉴权。
> **影响范围**: `cmd/server`（API Server 入站鉴权）+ 文档 `docs/MCP_BRIDGE.md`、`docs/MCP_TRANSPORT_BRIDGE_DESIGN.md`
> **背景**: 由 Acme 消费侧在准备接入 personal MCP 时端到端验证发现。

---

## 目录

- [一、Bug 摘要](#一bug-摘要)
- [二、代码证据](#二代码证据)
- [三、文档与实现的矛盾](#三文档与实现的矛盾)
- [四、为什么 sandrpod 自己的测试漏掉了](#四为什么-sandrpod-自己的测试漏掉了)
- [五、修复方案对比](#五修复方案对比)
- [六、推荐方案 A 详细设计](#六推荐方案-a-详细设计)
- [七、代码 Diff](#七代码-diff)
- [八、测试计划](#八测试计划)
- [九、文档更新清单](#九文档更新清单)
- [十、影响评估与发布说明](#十影响评估与发布说明)
- [十一、验收标准](#十一验收标准)

---

## 一、Bug 摘要

一句话：**API Server 的 `authMiddleware` 和 agent 的 `mcpTokenMiddleware` 都消费 `Authorization` HTTP header，但两者期望的值不同（前者要 `cfg.Token`，后者要 `--mcp-token`），导致客户端无法在同一个请求里同时通过两层鉴权。**

具体后果：

- 生产部署里 sandrpod API Server 几乎总会设 `cfg.Token`（否则任何能访问 `:8080` 的人都能拉起 sandbox）
- 同时 agent 也会启用 `--mcp-token`（否则任何能穿透 tunnel 的请求都能调用员工 PC 上的工具，已是 `docs/MCP_BRIDGE.md` 推荐配置）
- 远端 MCP 客户端只能在 `Authorization` 头里塞**一个**值——要么过 API Server 401（值 = `mcp_token`），要么过 agent 401（值 = `cfg.Token`）

`docs/MCP_BRIDGE.md` §Authentication 里描述的"API Server 透传 Authorization、自己不验证" 的产品意图**没有在代码里实现**。

---

## 二、代码证据

### 2.1 API Server 强制校验 `Authorization`

[`cmd/server/main.go:66-76`](../cmd/server/main.go)

```go
authMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if cfg.Token != "" {
            if r.Header.Get("Authorization") != "Bearer "+cfg.Token {
                http.Error(w, "Unauthorized", http.StatusUnauthorized)
                return
            }
        }
        next(w, r)
    }
}
```

只要 `cfg.Token` 非空，所有被 `authMiddleware` 包裹的路由都要求 `Authorization: Bearer ${cfg.Token}`。

### 2.2 MCP 路由完全在 authMiddleware 内

[`cmd/server/main.go:591`](../cmd/server/main.go)

```go
mux.HandleFunc("/api/v1/sandboxes/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
    // ... path parsing ...

    // line 679 起：
    if action == "mcp" || strings.HasPrefix(action, "mcp/") {
        _, t, ok := sandboxTunnel(name, sandboxStore, tunnelStore, directStore, w)
        if !ok {
            return
        }
        subPath := strings.TrimPrefix(path, name+"/")
        targetURL := "http://agent/" + subPath
        if r.URL.RawQuery != "" {
            targetURL += "?" + r.URL.RawQuery
        }
        proxyHTTPStreaming(t, r, targetURL, w)
        return
    }
    // ...
}))
```

`/api/v1/sandboxes/{name}/mcp` 进入处理函数时，`Authorization` 已经被 authMiddleware 校验过——必须等于 `cfg.Token`。

### 2.3 `proxyHTTPStreaming` 头透传但被消耗的 Authorization 没救

[`cmd/server/main.go:1226-1264`](../cmd/server/main.go)

```go
func proxyHTTPStreaming(t *tunnel.PoderTunnel, r *http.Request, targetURL string, w http.ResponseWriter) {
    req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
    // ...
    for k, v := range r.Header {
        if k != "Host" {
            req.Header[k] = v   // 透传所有头，包括 Authorization
        }
    }
    resp, err := t.StreamClient().Do(req)
    // ...
}
```

代码本身**确实**透传 `Authorization`，所以 agent 收到的 Authorization 就是 `cfg.Token`——但这恰好不是 agent `mcpTokenMiddleware` 想验证的 `--mcp-token`，所以 agent 这一关也会 401。

### 2.4 Agent 侧也只认 Authorization

[`cmd/agent/mcp_auth.go:36-55`](../cmd/agent/mcp_auth.go)

```go
func mcpTokenMiddleware(expectedToken string, next http.Handler) http.Handler {
    if expectedToken == "" {
        return next
    }
    want := []byte(expectedToken)
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        got := extractBearer(r.Header.Get("Authorization"))
        if got == "" || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
            w.Header().Set("WWW-Authenticate", `Bearer realm="sandrpod-mcp"`)
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

`extractBearer(r.Header.Get("Authorization"))` —— 同一个 header 是 agent 侧鉴权的**唯一**入口。

### 2.5 总结：单一头无解

|  | API Server 期望 | Agent 期望 |
|---|---|---|
| 头名 | `Authorization` | `Authorization` |
| 值 | `Bearer ${cfg.Token}` | `Bearer ${mcp_token}` |
| 谁先看 | 先 | 后（穿过 tunnel 后） |

两个值在生产部署里必然不同（一个是 corp 级 sandrpod tunnel 鉴权 token，一个是 sandbox 级 MCP 端点 Bearer），单一 `Authorization` 头无法同时满足两端。

---

## 三、文档与实现的矛盾

[`docs/MCP_BRIDGE.md` §Authentication](MCP_BRIDGE.md) 当前写：

> The API Server forwards the `Authorization` header verbatim — it does
> **not** know or validate the secret. So even a compromised API Server
> cannot forge new MCP calls; it can only replay ones it intercepted.

这段描述**与实现矛盾**。代码里 API Server 在 `cfg.Token != ""` 时**会**验证 `Authorization`，验证失败直接 401，根本到不了 forward 步骤。

矛盾的根源可能是：

- 作者写文档时假设 API Server 用其它头（如 `X-Sandrpod-Token`）鉴权
- 但 `authMiddleware` 是 sandrpod 早期就有的代码（在 MCP bridge 之前），用的就是 `Authorization`
- 引入 MCP bridge 时**没有解决两个 Authorization 的优先级问题**

---

## 四、为什么 sandrpod 自己的测试漏掉了

[`cmd/agent/mcp_security_test.go`](../cmd/agent/mcp_security_test.go) 测试得很充分，但只覆盖 agent 侧 `mcpTokenMiddleware` —— 用 `httptest.NewServer(mcpTokenMiddleware(...))` 跳过了 API Server。

`cmd/server/server_test.go` 和 `cmd/server/reconcile_test.go` `grep -n mcp` 全无匹配——**完全没有**端到端的"客户端 → API Server → tunnel → agent /mcp"集成测试。

这是测试覆盖盲区。修复同时要补这个 E2E case，否则同样的 bug 还会以其它形式回来。

---

## 五、修复方案对比

| 方案 | 改动量 | 向后兼容 | 安全模型清晰度 | 推荐度 |
|---|---|---|---|---|
| **A. `authMiddleware` 加 `X-Sandrpod-Token` 替代头（保留 Authorization 作旧兼容）** | ~15 LOC + 测试 | ✅ 全兼容 | ✅ 语义分层清晰 | ⭐ **推荐** |
| **B. MCP 路由从 authMiddleware 中剥离，只靠 agent 端 `--mcp-token` 鉴权** | ~10 LOC + 测试 | ✅ 兼容（但 `--mcp-token` 变事实必填） | ⚠ API Server 这层不再卡 sandbox 名遍历 | 备选 |
| **C. `authMiddleware` 改用 `X-Sandrpod-Token`（替换，不并存）** | ~5 LOC | ❌ 破坏现有客户端 | ✅ 干净 | ❌ 不推荐 |
| **D. 不修复，要求客户端复用同一 token（`cfg.Token == mcp_token`）** | 0 | — | ❌ 失去 defense-in-depth | ❌ 不推荐 |

### 方案 B 的隐藏代价

剥离 MCP 路由意味着任何能 hit API Server 公网端口的人都能尝试调用 `/api/v1/sandboxes/{name}/mcp`：

- 不知道 sandbox name 的话会被 404 挡住，但 sandbox name 容易枚举（员工名 hash 拼接）
- 如果某员工没启 `--mcp-token`（"我是本地 dev 就关了"），他的整个 PC 工具集对外是裸的
- 失去"API Server 这一层至少能挡住未授权访问"的防御

方案 A 保留 API Server 这一层的访问控制 + agent 一层的资源 Bearer，两层分工清晰。

### 方案 D 的死穴

设 `cfg.Token == mcp_token` 意味着：

- corp 级管理员能调任何员工的任何 MCP 工具（cfg.Token 是 corp 共享的）
- 完全失去了 `--mcp-token` 设计时想要的 "API Server 被入侵也只能 replay" 的防御纵深
- 这违反了 sandrpod 自己 [`docs/MCP_BRIDGE.md` 里说的](MCP_BRIDGE.md) "even a compromised API Server cannot forge new MCP calls"

---

## 六、推荐方案 A 详细设计

### 6.1 语义

| Header | 作用 | 谁用 |
|---|---|---|
| `X-Sandrpod-Token` | 对 sandrpod API Server **自己**鉴权 | 任何调 `/api/v1/*` 的客户端，**首选** |
| `Authorization: Bearer <secret>` | 对 sandrpod API Server 自己鉴权 | 旧客户端（兼容）/ 透传到资源层（MCP agent） |

新规则：

1. `authMiddleware` 优先检查 `X-Sandrpod-Token`，匹配则放行（**不**消耗 `Authorization`）
2. `X-Sandrpod-Token` 未提供（或不匹配）才回退检查 `Authorization`，匹配则放行
3. 都不匹配返回 401
4. 当 `X-Sandrpod-Token` 鉴权通过时，`Authorization` 原封不动透传到后端（agent）
5. 当 `Authorization` 鉴权通过时，沿用旧行为（透传给 agent，但此时 agent 收到的就是 `cfg.Token`，agent 侧 `--mcp-token` 若启用大概率不会匹配——这只对旧客户端有意义，老调用者本来也没用 MCP）

### 6.2 客户端调用模式（推荐）

```http
GET /api/v1/sandboxes/john-mbp/mcp/manifest HTTP/1.1
Host: sandrpod.example.com
X-Sandrpod-Token: <cfg.Token, sandrpod API Server 自己的鉴权>
Authorization: Bearer <mcp_token, 透传给 agent>
```

API Server 看 `X-Sandrpod-Token` 放行；agent 收到的 `Authorization` 是 `mcp_token`，自己的 `mcpTokenMiddleware` 验证通过。两层鉴权独立可见、独立轮换。

### 6.3 旧客户端调用（继续工作）

```http
GET /api/v1/sandboxes/john-mbp HTTP/1.1
Authorization: Bearer <cfg.Token>
```

走老路径，沿用现有所有非 MCP CRUD 调用——零改动。

### 6.4 安全模型

| 威胁 | 是否被缓解 |
|---|---|
| 公网攻击者直接打 `/api/v1/*` | ✅ X-Sandrpod-Token 或 Authorization 必须正确 |
| API Server 被入侵 + 想伪造 MCP 调用 | ✅ 不知 `mcp_token` 仍打不进 agent /mcp（mcp_token 只在 Acme ↔ 员工 PC 链路出现，API Server 进程内不持有） |
| API Server 被入侵 + replay 截获的 MCP 请求 | ⚠ 仍可 replay（与现有"Authorization 透传"模型一致，是已知接受的残余风险） |
| 旧客户端用 Authorization | ✅ 路径仍兼容，但已不该再这么写新代码 |

---

## 七、代码 Diff

### 7.1 `cmd/server/main.go` — authMiddleware

```diff
-    // Authentication middleware
+    // Authentication middleware
+    //
+    // Prefer X-Sandrpod-Token header for new clients: it leaves the
+    // Authorization header free to be forwarded verbatim to the agent
+    // (used by /api/v1/sandboxes/{name}/mcp to carry the MCP Bearer).
+    //
+    // Legacy: Authorization: Bearer <cfg.Token> is still accepted as the
+    // sandrpod API token, but new code SHOULD switch to X-Sandrpod-Token.
     authMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
         return func(w http.ResponseWriter, r *http.Request) {
-            if cfg.Token != "" {
-                if r.Header.Get("Authorization") != "Bearer "+cfg.Token {
-                    http.Error(w, "Unauthorized", http.StatusUnauthorized)
-                    return
-                }
+            if cfg.Token == "" {
+                next(w, r)
+                return
+            }
+
+            // Preferred path: X-Sandrpod-Token. Leaves Authorization free
+            // for the resource layer (e.g. agent /mcp Bearer).
+            if subtle.ConstantTimeCompare(
+                []byte(r.Header.Get("X-Sandrpod-Token")),
+                []byte(cfg.Token),
+            ) == 1 {
+                next(w, r)
+                return
+            }
+
+            // Legacy fallback: Authorization: Bearer <cfg.Token>.
+            // When this path is taken, Authorization will reach the agent
+            // as cfg.Token rather than the MCP Bearer — old clients have
+            // no MCP needs anyway, so this is acceptable.
+            if authHeader := r.Header.Get("Authorization"); authHeader != "" {
+                want := "Bearer " + cfg.Token
+                if subtle.ConstantTimeCompare([]byte(authHeader), []byte(want)) == 1 {
+                    next(w, r)
+                    return
+                }
             }
+
+            w.Header().Set("WWW-Authenticate",
+                `Bearer realm="sandrpod-api", X-Sandrpod-Token realm="sandrpod-api"`)
+            http.Error(w, "Unauthorized", http.StatusUnauthorized)
+            return
-            next(w, r)
         }
     }
```

记得 import `"crypto/subtle"`。

### 7.2 `cmd/server/main.go` — 关键 MCP 路由确保不残留旧 Authorization 验证

无需改 MCP 路由本身，只要 authMiddleware 修好就生效。`proxyHTTPStreaming` 现有的"透传所有头"行为正是我们想要的。

### 7.3 客户端 SDK 同步（可选）

[`pkg/sdk/`](../pkg/sdk/) 下的 Go / Python SDK 加 `X-Sandrpod-Token` 支持。Go 端：

```go
// pkg/sdk/go/client.go (假设路径)
type Client struct {
    apiToken string
    // ...
}

func (c *Client) authHeaders() http.Header {
    h := http.Header{}
    h.Set("X-Sandrpod-Token", c.apiToken)
    return h
}
```

Python 端 (`pkg/sdk/python/`) 同理。MCP 调用时不要把 apiToken 写到 Authorization，把 mcp_token 写到 Authorization：

```python
# 调 /mcp endpoint 时
headers = {
    "X-Sandrpod-Token": api_token,
    "Authorization": f"Bearer {mcp_token}",
}
```

---

## 八、测试计划

### 8.1 单测：authMiddleware 新增 cases

在 `cmd/server/server_test.go` 新建 `TestAuthMiddleware`：

| Case | X-Sandrpod-Token | Authorization | cfg.Token | 期望 |
|---|---|---|---|---|
| 无 token 配置全放行 | (any) | (any) | `""` | 200 |
| X-Sandrpod-Token 正确 | `s3cret` | `Bearer mcp-token-xyz` | `s3cret` | 200，Authorization 应原值透传 |
| X-Sandrpod-Token 正确 + 无 Authorization | `s3cret` | (none) | `s3cret` | 200 |
| Authorization 正确（兼容） | (none) | `Bearer s3cret` | `s3cret` | 200 |
| 两个头都没有 | (none) | (none) | `s3cret` | 401 + WWW-Authenticate |
| X-Sandrpod-Token 错 + Authorization 错 | `wrong` | `Bearer wrong` | `s3cret` | 401 |
| X-Sandrpod-Token 错 + Authorization 正确 | `wrong` | `Bearer s3cret` | `s3cret` | 200（fallback 生效） |
| X-Sandrpod-Token 正确 + Authorization 是 MCP token | `s3cret` | `Bearer mcp-xyz` | `s3cret` | 200，**关键**：进入 handler 后 r.Header["Authorization"] 仍等于 `Bearer mcp-xyz` |

最后一个 case 是这次修复的灵魂——必须断言"经过 authMiddleware 后 Authorization 没有被改写"。

### 8.2 E2E：透传到 agent 验证

新建 `cmd/server/mcp_e2e_test.go`（如果之前没有），用 httptest 模拟一个完整链路：

```go
func TestMCPRouteForwardsBearerToAgent(t *testing.T) {
    // 1. 启动一个 fake agent HTTP server，它的 /mcp handler 断言
    //    收到的 Authorization == "Bearer mcp-token-xyz"
    // 2. 模拟一个 tunnel 把 fake agent 注入 directStore
    // 3. 用 buildMux 起 API Server，cfg.Token = "api-token-abc"
    // 4. 客户端用 X-Sandrpod-Token: api-token-abc + Authorization: Bearer mcp-token-xyz
    //    调 /api/v1/sandboxes/test-sandbox/mcp/manifest
    // 5. 断言:
    //    - API Server 放行
    //    - fake agent 收到的 Authorization 正确是 mcp-token-xyz
    //    - 响应 200 透回客户端
}
```

这是补盲点：之前 sandrpod 完全没有 E2E case 覆盖 "client → API Server → tunnel → agent /mcp"。

### 8.3 集成测试：手工烟雾

修复合入后跑一次：

```bash
# Terminal 1: 起 API Server
go run ./cmd/server -port 8080 -token api-token-abc

# Terminal 2: 起 agent（mcp.json 里至少有一个 server）
sandrpod-agent \
  -api-url=http://localhost:8080 \
  -name=test-sandbox \
  -token=api-token-abc \
  -mcp-token=mcp-token-xyz \
  -mcp-config=./test_mcp.json

# Terminal 3: 客户端测试
curl -v http://localhost:8080/api/v1/sandboxes/test-sandbox/mcp/manifest \
  -H "X-Sandrpod-Token: api-token-abc" \
  -H "Authorization: Bearer mcp-token-xyz"
# 期望 200 + manifest JSON

curl -v http://localhost:8080/api/v1/sandboxes/test-sandbox/mcp/manifest \
  -H "X-Sandrpod-Token: api-token-abc" \
  -H "Authorization: Bearer WRONG"
# 期望 401（agent 侧 mcpTokenMiddleware 卡住）

curl -v http://localhost:8080/api/v1/sandboxes/test-sandbox/mcp/manifest \
  -H "X-Sandrpod-Token: WRONG" \
  -H "Authorization: Bearer mcp-token-xyz"
# 期望 401（API Server 侧 authMiddleware 卡住）

# 旧客户端兼容
curl -v http://localhost:8080/api/v1/sandboxes \
  -H "Authorization: Bearer api-token-abc"
# 期望 200（旧的 sandbox list 调用仍工作）
```

---

## 九、文档更新清单

修复合入同时需要更新：

### 9.1 `docs/MCP_BRIDGE.md`

**§Authentication 段重写**：明确两层 token 各自的 header。

> ### Authentication
>
> The bridge involves two independent tokens:
>
> | Token | Set on | Carried in | Purpose |
> |---|---|---|---|
> | API Server token (`cfg.Token`) | API Server startup `-token` flag | `X-Sandrpod-Token: <token>` | Authenticates the caller to the sandrpod API Server itself |
> | MCP Bearer (`--mcp-token`) | `sandrpod-agent --mcp-token=<token>` | `Authorization: Bearer <token>` | Authenticates the caller to the agent's `/mcp` endpoint (forwarded verbatim through the tunnel) |
>
> The API Server validates `X-Sandrpod-Token` and forwards `Authorization`
> untouched, so a compromised API Server can replay captured MCP calls
> but cannot forge new ones (it never sees the MCP Bearer in plaintext).
>
> **Legacy clients** that put the API token in `Authorization: Bearer` still work,
> but they conflict with MCP routes — for any client that touches `/mcp`,
> switch to `X-Sandrpod-Token`.

LangChain 示例代码也要同步修：

```python
client = MultiServerMCPClient({
    "personal": {
        "url": "https://your-server/api/v1/sandboxes/laptop-1/mcp",
        "transport": "streamable_http",
        "headers": {
            "X-Sandrpod-Token": "api-token-abc",      # 新增
            "Authorization": "Bearer mcp-token-xyz",   # 现有
        },
    },
})
```

### 9.2 `docs/MCP_TRANSPORT_BRIDGE_DESIGN.md`

§七 "鉴权" 子段补一段说明 API Server 用 `X-Sandrpod-Token`，Authorization 透传。

### 9.3 `docs/PERMISSION_AND_AUDIT.md`

无需改动（这一改不影响 path-permission 流）。

### 9.4 CHANGELOG.md

```markdown
## [Unreleased]

### Changed

- **cmd/server**: `authMiddleware` now prefers `X-Sandrpod-Token` over
  `Authorization` for API Server authentication. This frees the
  `Authorization` header to be forwarded verbatim through the tunnel,
  enabling the documented two-tier MCP authentication model. Existing
  clients using `Authorization: Bearer <cfg.Token>` continue to work
  for non-MCP routes; **for any client calling `/mcp` routes, switch to
  `X-Sandrpod-Token`** to avoid conflicting with the MCP Bearer.

### Fixed

- MCP transport bridge: `--mcp-token` Bearer is now actually reachable
  through the API Server tunnel proxy. Previously `cfg.Token != ""`
  deployments could not pass two different Authorization values, making
  documented two-tier auth unusable.
```

### 9.5 README

主 `README.md` 如果有"API token 用 Authorization Bearer"的例子，也要更新成 X-Sandrpod-Token 示范。

---

## 十、影响评估与发布说明

### 10.1 影响面

- **Sandrpod 内部 Go SDK**：需要小改（option 7.3）
- **Sandrpod Python SDK** (`pkg/sdk/python/`): 同上
- **sandrpod-tray**：本地 Unix socket 通信不走 HTTP Authorization，**零影响**
- **既有部署的 Sandbox CRUD 客户端**：用 `Authorization: Bearer` 写法继续工作，**零影响**
- **Acme 平台**：从 0.1 设计稿开始就走新 header，**直接受益**——可以解锁 personal MCP 集成
- **任何 MCP 客户端**：需要在 sandrpod 升级后切到新 header 才能调 `/mcp`

### 10.2 发布建议

- Patch 版本号（`v0.x.y` → `v0.x.(y+1)`），因为对外行为是**新增**接受方式，不是破坏
- CHANGELOG 用 "Changed" 而非 "Breaking" 标签
- 发布说明明确建议消费方在 1-2 个 minor 版本内迁移到 `X-Sandrpod-Token`，后续 major 版本可考虑废弃 Authorization fallback

### 10.3 回滚

如果发现修复引入回归（例如旧客户端因为 `WWW-Authenticate` 头变了出问题），revert 这单个 commit 即可，无 schema / 数据迁移依赖。

---

## 十一、验收标准

修复合入主干前必须满足：

- [ ] `cmd/server/main.go` authMiddleware 按 §七 diff 改造，引用 `crypto/subtle` 做常量时间比较
- [ ] §8.1 八条单测全部通过
- [ ] §8.2 E2E 测试新增并通过——这是这个修复的核心验证点
- [ ] §8.3 手工烟雾测试三条 curl 行为符合预期
- [ ] `docs/MCP_BRIDGE.md` §Authentication 重写完成
- [ ] `docs/MCP_TRANSPORT_BRIDGE_DESIGN.md` §七 更新
- [ ] `CHANGELOG.md` 加 Changed + Fixed 条目
- [ ] Sandrpod Go SDK + Python SDK 调用样例切到 `X-Sandrpod-Token`
- [ ] 消费方（Acme 等）收到 PR/release notification，知道可以切换

---

## 附录 A：为什么不顺便去掉 Authorization fallback

短期保留 Authorization 兼容路径的好处：

- 消费方升级时间窗口
- 现有 CI / 运维脚本 / curl alias 不会一夜炸掉
- 修复本身的 blast radius 最小化

长期（半年到一年）可以在某个 major 版本里下线 Authorization fallback，强制只接受 `X-Sandrpod-Token`，那时再发一份独立的 deprecation note。

## 附录 B：相关讨论与来源

- 由 Acme 消费侧在准备接入 personal MCP 时端到端验证发现
- Acme 侧设计稿：[`PLATFORM_PERSONAL_MCP_INTEGRATION.md`](https://example.invalid/PLATFORM_PERSONAL_MCP_INTEGRATION.md) §6.2（标了 TODO 等待此修复）
- 配套文档：[`MCP_TRANSPORT_BRIDGE_DESIGN.md`](MCP_TRANSPORT_BRIDGE_DESIGN.md)（最初设计稿，鉴权章节假设过于乐观，本修复矫正之）

---

*Last Updated: 2026-05-28*
*Status: 待实施*
*Maintainer: sandrpod 维护者（实施）+ Acme 平台团队（发现者）*
