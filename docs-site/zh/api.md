---
title: REST API
description: 原生控制面 API —— 沙箱、worker、执行、出网，以及怎么鉴权。
---

# REST API

基础地址就是你运行控制面的地址。下面所有端点都说 JSON，脚本、SDK、`curl` 都能调。

## 鉴权

接受两种头，含义不同：

| 头 | 凭据 | 范围 |
|---|---|---|
| `X-Sandrpod-Token` | 平台 token | 全部，所有工作区 |
| `X-Sandrpod-Token` | `sk_…` 工作区 key | 单个工作区 |
| `X-Sandrpod-Token` | `e2b_…` E2B key | 单个工作区（同一份凭据的 E2B 拼写） |
| `Cookie: sandrpod_session` | 浏览器会话 | 已登录的工作区 |

平台 token 是运维凭据：能列出所有沙箱、删掉所有 worker。**不要把它放进智能体**。

## 沙箱

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/v1/sandboxes` | 创建。`async:true` 返回 `202` 加 job id |
| `GET` | `/api/v1/sandboxes` | 列表，按凭据限定范围 |
| `GET` | `/api/v1/sandboxes/{name}` | 单个沙箱 |
| `DELETE` | `/api/v1/sandboxes/{name}` | 删除记录和容器 |
| `POST` | `/api/v1/sandboxes/{name}/start` 或 `/stop` | 恢复 / 暂停 |
| `POST` | `/api/v1/sandboxes/execute?sandbox={name}` | 执行代码 |
| `GET` | `/api/v1/sandboxes/stream?sandbox={name}` | 流式执行 |
| `GET` | `/api/v1/sandboxes/{name}/toolbox/*` | 转发到沙箱内的 toolbox |
| `POST` | `/api/v1/sandboxes/{name}/snapshot` | 提交成镜像 |

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
| `DELETE` | `/api/v1/poders/{id}` | 删除记录（并断开隧道） |
| `POST` | `/api/v1/poders/{id}/heartbeat` | 仅 worker：一个 worker 只能心跳自己那条记录 |
| `GET` | `/ws/poder/connect` | worker 拨入的 WebSocket（不是手工调的） |

## 出网

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/sandboxes/{name}/egress` | 当前策略，未设置时 `null` |
| `PUT` | `/api/v1/sandboxes/{name}/egress` | 替换策略（会校验） |
| `DELETE` | `/api/v1/sandboxes/{name}/egress` | 取消设置 —— 和 `PUT {}` 不是一回事 |
| `POST` | `/api/v1/policy/decide` | 单连接裁决，供 `-egress-mode=decider` 使用 |

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
`/sandboxes`、`/sandboxes/{id}`、`/sandboxes/{id}/pause`，以及
`<端口>-<id>.<域名>` 上的 envd RPC。