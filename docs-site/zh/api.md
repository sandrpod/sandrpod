---
title: REST API
description: 原生控制面 API —— 沙箱、worker、执行、出网，以及怎么鉴权。
---

# REST API

基础地址就是你运行控制面的地址。下面所有端点都说 JSON，脚本、SDK、`curl` 都能调。

## 鉴权

接受以下几种凭据，含义不同：

| 头 | 凭据 | 范围 |
|---|---|---|
| `X-Sandrpod-Token` | 平台 token | 全部，所有工作区 |
| `X-Sandrpod-Token` | `sk_…` 工作区 key | 单个工作区 |
| `X-Sandrpod-Token` | `e2b_…` E2B key | 单个工作区（同一份凭据的 E2B 拼写） |
| `Cookie: sd_cloud_session` | 浏览器会话 | 已登录的工作区 |

平台 token 是运维凭据：能列出所有沙箱、删掉所有 worker。**不要把它放进智能体**。

## 沙箱

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/v1/sandboxes` | 创建。`async:true` 返回 `202` 加 job id |
| `GET` | `/api/v1/sandboxes` | 列表，按凭据限定范围；`limit`/`offset`，响应带 `has_more` |
| `GET` | `/api/v1/sandboxes/{name}` | 单个沙箱 |
| `DELETE` | `/api/v1/sandboxes/{name}` | 删除记录和容器 |
| `POST` | `/api/v1/sandboxes/{name}/start` 或 `/stop` | 恢复 / 暂停 |
| `POST` | `/api/v1/sandboxes/execute?sandbox={name}` | 执行代码 |
| `GET` | `/api/v1/sandboxes/stream?sandbox={name}` | 流式执行 |
| `GET` | `/api/v1/sandboxes/{name}/logs?tail=N` | 容器日志 |
| `GET` | `/api/v1/sandboxes/{name}/toolbox/*` | 转发到沙箱内的 toolbox |
| `POST` | `/api/v1/sandboxes/{name}/snapshot` | 提交成镜像 |
| `GET` | `/api/v1/snapshots` | 在线 worker 上的快照镜像（按 tag 去重） |
| `DELETE` | `/api/v1/snapshots?image={repo:tag}` | 删除一个（带 `poder_id` 可只删某个 worker 上的） |

`{name}` 是创建时填的名字，**只在工作区内唯一**：两个工作区都可以有 `mysandbox`，
请求会按调用凭据所属的工作区解析。每个沙箱对象还带 `routing_id`，它是服务端全局唯一
的路由键，预览 Host（`<端口>-<routing_id>.<域名>`）用的就是它；不要自己拼 URL，
用 `/preview/{port}` 签发。

创建请求体：

```json
{
  "name": "agent-01",
  "provider_type": "local",
  "region": "local",
  "cpu_cores": 1,
  "memory_mb": 1024,
  "ttl_seconds": 3600,
  "network": { "allowed_domains": ["pypi.org", "github.com"] }
}
```

带 `network` 策略、而机队里没有具备出网能力的 worker 时，返回 **409**，而不是启动一个
没有实际限制的沙箱。

创建请求体还接受 `env_vars`（写到容器环境里）、`storage_mb`（工作区存储预留）、
`labels`、`private_ports: true`（该沙箱所有公共端口都要求凭证）、`persist_data`、
`gpu`、`runtime`（`runc` 或 `gvisor`）、`poder_id`（跳过调度）和 `async`。省略 CPU/内存/存储时默认 1 vCPU /
1024 MiB / 2048 MiB。

对沙箱的长连接与附加能力走同一前缀：`/api/v1/sandboxes/{name}/session/*`（长驻
shell 会话）、`/mcp`（原生 MCP bridge）、`/pty`（原始终端）、`/toolbox/*`（文件、
进程、统计）。CLI 已经封装这些路径；自己写客户端时它们也是稳定的。

`GET /api/v1/jobs/{id}` 返回异步创建的进度、错误和结果；同一路径的 `PATCH` 是
worker 上报状态，需要管理员凭据。

## 执行

```bash
curl -s -X POST "$API/api/v1/sandboxes/execute?sandbox=agent-01" \
  -H "X-Sandrpod-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"language":"bash","code":"echo hi","timeout":20}'
```

```json
{"exit_code":0,"stdout":"hi\n","stderr":"","started_at":"…","ended_at":"…"}
```

`language` 取 `bash`、`python`、`node`、`sh`。`timeout` 单位是秒。

## Worker

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/poders` | 清点：状态、资源、能力、地域 |
| `POST` | `/api/v1/poders/{id}/sandboxes` | 直接在该 worker 上创建，跳过调度 |
| `DELETE` | `/api/v1/poders/{id}` | 删除记录（并断开隧道） |
| `POST` | `/api/v1/poders/{id}/heartbeat` | 仅 worker：一个 worker 只能心跳自己那条记录 |
| `POST` | `/api/v1/poders/enroll` | worker 用加入邀请换取自己的 worker key |
| `GET` | `/ws/poder/connect` | worker 拨入的 WebSocket（不是手工调的） |

## 出网

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/sandboxes/{name}/egress` | 当前策略，未设置时 `null` |
| `PUT` | `/api/v1/sandboxes/{name}/egress` | 替换策略（会校验） |
| `DELETE` | `/api/v1/sandboxes/{name}/egress` | 取消设置 —— 和 `PUT {}` 不是一回事 |
| `POST` | `/api/v1/policy/decide` | 单连接裁决，供 `-egress-mode=decider` 使用 |

## Token 签发

仅管理员可用，且服务端必须用持久化 `-db` 启动。签发的 key 采用 `e2b_<hex>` 拼写，
同一把 key 在原生面和 E2B 面都能用；服务端只存哈希，裸 key 只在签发时返回一次。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/tokens` | 列出名称、前缀、角色和创建时间 |
| `POST` | `/api/v1/tokens` | `{"name":"…","role":"user"\|"admin"}` → `201` 加裸 key |
| `DELETE` | `/api/v1/tokens/{prefix}` | 按打印出的前缀吊销 |

托管部署可能关闭这个路由，改由控制台按工作区签发 key。

## 运维

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/health` | 存活与版本 |
| `GET` | `/healthz` | 更深的检查：数据库、worker、支付、磁盘 |
| `GET` | `/metrics` | Prometheus；需要管理员凭据 |
| `GET` | `/api/v1/capabilities` | 这个控制面支持什么（`egress_policy` 等） |

## 错误码

| 状态码 | 含义 |
|---|---|
| `400` | 请求体格式错误，或名字不合法 |
| `401` | 没有凭据，或凭据不认识 |
| `403` | 凭据有效但无权（worker 想读机队视图、member 想花钱） |
| `404` | 没有这个沙箱 |
| `409` | 冲突：名字被占、沙箱不是 `RUNNING`、或某个能力没有 worker 具备 |
| `429` | 被限流；`Retry-After` 告诉你要等多久 |
| `503` | 拥有该沙箱的 worker 没有连在任何实例上 |

## E2B 兼容面

可以在独立域名上再挂一个 E2B 形状的面，见 [E2B 直接替换](/zh/e2b)：路径是
`/sandboxes`、`/sandboxes/{id}`、`/sandboxes/{id}/pause`、`/snapshots`、
`/sandboxes/{id}/fork`、`/sandboxes/{id}/network`，以及
`<端口>-<id>.<域名>` 上的 envd RPC。

## OpenAPI

原生面不提供 OpenAPI 文档；本页和 CLI 就是契约。E2B 面遵循 E2B 自己的 schema
（上游 `spec/openapi.yml`），这正是它能做直接替换的原因。