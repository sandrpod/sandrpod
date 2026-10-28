---
title: 多实例
description: 跑在 PostgreSQL 上的负载均衡控制面 —— 什么被转发、什么被投递、什么能在崩溃中活下来。
---

# 多实例

N 个控制面实例挂在一个负载均衡后面，共享一个 PostgreSQL，每个实例有自己的
`-node-url`（对端能访问到的内网地址）。

```bash
./server -db postgres://… -port 8080 -node-url http://10.0.1.5:8080   # A
./server -db postgres://… -port 8080 -node-url http://10.0.1.6:8080   # B
```

一个 worker 拨向其中一个实例，它的隧道就活**在那个进程的内存里**。数据库只记录哪个
节点持有它（`tunnel_owners`，每次心跳续期，可信 90 秒）。下面所有事情都源于这一条。

## 什么会跨实例

| 请求 | 落在错误节点时的行为 |
|---|---|
| `POST /sandboxes/execute` | **转发**给持有隧道的节点 |
| `/sandboxes/{name}/toolbox/*`（文件、PTY、流） | **转发** |
| E2B 面 | **转发** |
| `POST /api/v1/sandboxes`（创建） | 由持有隧道的节点**投递**（`POST /internal/provision`） |

执行和代理路径转发整个请求：对着持有连接的那个节点做一次无状态重放。

创建不能重放 —— 它会扣额度、写记录、记 job —— 所以只有它的**最后一步**跨网络。接收
请求的实例做完到容器之前的所有事，然后请持有隧道的节点把它推下去。

**不需要粘性路由**，这正是几十台 worker 的机队能用的原因。

## 创建能在"接单的进程"死掉后活下来

"接受"和"投递"是两步，进程可能死在中间。载荷在尝试投递之前就落进 `jobs.request`，
所以任何实例都能把它做完：

| 步骤 | 落地的东西 |
|---|---|
| 接受 | 沙箱记录、job 行，以及要发出去的精确载荷 |
| 投递 | 成功后 job 变 `COMPLETED` |
| 恢复 | 任何实例发现超过 45 秒的 `PENDING` 创建，认领并投递 |

认领是对 `jobs.updated_at` 的 compare-and-swap，所以 N 个实例里恰好一个投递。语义是
**至少一次**，worker 能容忍重投，因为 pode 是用沙箱名给容器命名的。

如果沙箱记录已经不在了，或者已经是 `ERROR` / `STOPPED`，job 会被判失败而不是重试：
重试一个被删除的沙箱，会让它在每一轮都复活一次。

## 必须成立的前提

| | |
|---|---|
| 数据库 | 每个实例都用 PostgreSQL；SQLite 只支持单实例 |
| `-node-url` / `CLOUD_NODE_URL` | 每个实例都要有，且和 `tunnel_owners` 里出现的字符串一致 |
| 凭据 | 所有实例用同一套签发材料，否则 A 发的会话会被 B 拒绝 |
| `/internal/` | 必须到达控制面，而不是 SPA 的 catch-all —— 给未知路径返回 `index.html` 的门户会用 HTML 应答对端 |
| 限流 | 计数是每实例独立的：N 个实例意味着 N 倍限额 |
| 迁移 | 在新实例启动前跑一次；它们只做加法 |

## 验证它

```bash
# owner 记录必须存在且新鲜
psql -tAc "select tunnel_key, node_url, updated_at, now() from tunnel_owners"

# 在 A 上执行，从 B 上执行同一句 —— 第二次必须是转发
curl -s -X POST http://A/api/v1/sandboxes/execute?sandbox=… -d '{"language":"bash","code":"echo ok"}'
curl -s -X POST http://B/api/v1/sandboxes/execute?sandbox=… -d '{"language":"bash","code":"echo ok"}'
```

第二次返回 `503 sandbox tunnel not available`，说明 owner 记录过期了：检查持有方实例的
`-node-url` 是否非空、是否在收心跳。

仓库里的 `hack/multi-instance-check.sh` 把这一切端到端跑一遍 —— 两个实例、一个数据库、
一个真实 worker、一条人为制造的丢失投递 —— 并且跑在 CI 里。
