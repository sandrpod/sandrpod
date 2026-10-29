---
title: Worker 与隧道
description: 一台机器如何加入机队、它宣告了什么、它能做什么。
---

# Worker 与隧道

Worker 是跑沙箱的机器。它通过**主动拨出**加入，所以永远不需要入站端口、公网地址，
也不需要开防火墙。

## 加入

两种形状，回答两个不同的问题。

```bash
# 在控制面：为一个工作区签一张接入凭证
curl -s -X POST "$API/portal/v1/join-tokens" \
  -H "X-Sandrpod-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"worker-1"}'
```

```json
{"secret":"jn_…",
 "command":"curl -fsSL https://docs.sandrpod.com/install.sh | sudo bash -s -- --api-url=… --token=jn_… --yes",
 "docker_run":"docker run -d --name sandrpod-worker …",
 "agent_command":"curl -fsSL https://docs.sandrpod.com/agent.sh | sudo bash -s -- … --name=device-worker-1"}
```

| 这台机器 | 用哪个 | 区别 |
|---|---|---|
| 装了 Docker 的服务器 | `install.sh`（worker） | 它承载**很多**沙箱容器；脚本负责 Docker、数据盘、镜像源、swap 和沙箱需要的句柄上限 |
| 笔记本、构建机、GPU 主机 | `agent.sh`（device） | 机器**本身就是**沙箱：它自己的文件、工具、GPU，中间没有容器 |
| 已经装好 Docker | `docker_run` | 同一个 worker，省掉它本来会跳过的步骤 |

两个脚本都是幂等的，结束时都会打印怎么看日志、怎么卸载。用同一张凭证，因为 worker
和 agent 都是 worker 角色。

**这个区别很重要。** worker 把每个沙箱隔离在容器里，一台能放十几个；agent 除了操作系统
本身没有任何额外隔离——对"整台机器只服务一个智能体"的场景正是你要的，对共享机队则正好
相反。

**最容易踩的是地域。** CLI 和 E2B 面默认都是 `local`；用 `-region=hk` 注册的 worker 对它们
不可见，创建会报 `no available local poder found`，而机队看起来一切正常。

## Worker 宣告了什么

握手时 worker 会带上身份、资源、**能力**和**租户**：

| 字段 | 效果 |
|---|---|
| `X-Poder-Capabilities: egress` | 只有这类 worker 能承载带出网策略的沙箱 |
| `X-Poder-Workspace: …` | 放置被限定：A 工作区的沙箱不会被调度到 B 工作区的 worker |
| 资源（架构、系统、核数、内存） | 记录在沙箱上，API 可以报告 |

如果请求带策略、而机队里没有具备能力的 worker，返回 **409**，而不是给你一个静默
"没有墙"的沙箱。

## 心跳与状态

worker 每十秒心跳一次，带上自己的容器列表。这做三件事：保持记录 `ONLINE`、让控制面
用"实际存在的容器"校正沙箱状态、以及（在多实例部署下）续期"哪个节点持有这条隧道"。

停止心跳的 worker 会变 `OFFLINE`；它的沙箱不再可达，落上去的创建会带着原因失败，而
不是挂着。

## 机器上的前置条件

| 要求 | 为什么 |
|---|---|
| Docker socket | worker 要创建容器 |
| `SANDRPOD_TOOLBOX_IMAGE` | 沙箱基础镜像，首次使用时拉取 |
| 预拉 `ghcr.io/sandrpod/egress-proxy` | 仅带出网策略的沙箱需要；没有它，创建会在**记录已经写入之后**失败 |
| 端口：无入站；出站 HTTPS + DNS | worker 要拨控制面、要拉镜像 |

磁盘是最先被吃满的资源：镜像加每个沙箱的可写层。一台 4 vCPU / 8 GB / 100 GB 的机器
能承载几个并发沙箱；默认沙箱是 1 核 1 GiB。

## 暂停不等于删除

停止沙箱只是暂停容器；它仍然占着 worker 的一个容器位，直到被删除。达到
`max-containers` 的 worker 会拒绝新的放置（`no available poder found`）—— 这是容量
回答，不是 bug。

## 查看机队

```bash
sandrpod-cli poder list          # id、状态、厂商、架构、地域
curl -s "$API/api/v1/poders" -H "X-Sandrpod-Token: $TOKEN" | jq
```

`poder delete <id>` 删除记录并断开隧道；`--keep-vm` 保留云主机以便排查。

## 下一步

- [隔离与放置](/zh/isolation) —— 决定哪个 worker 拿到哪个沙箱的规则。
- [多实例](/zh/multi-instance) —— 当控制面本身也是一支机队。
