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


### 设备 agent 能碰到什么

agent 不是容器：它和这台机器之间**没有文件系统边界**。这正是它的价值——用它自己的工具、
GPU 和文件——也正是它的风险。所以 `agent.sh` 默认把它关起来，而且每一层都是一个你能看见的
参数：

| 默认 | 作用 | 怎么改 |
|---|---|---|
| 以**执行 sudo 的那个用户**运行，不是 root | 笔记本不是构建服务器 | `--user` |
| `--work-dir ~/sandrpod-work` | 写入被限制在这里——外加系统临时目录（构建工具依赖它）；创建为 `0700` | `--work-dir` |
| `--permission-mode prompt` | 工作目录之外的操作会询问，没人回答就拒绝 | `--permission-mode strict\|off` |
| `--os-sandbox auto` | 内核级：根文件系统只读，只能写到工作目录（macOS 用 seatbelt，Linux 用 bwrap） | `--os-sandbox off` |

`--full-machine` 把这一切打开——`/`、无门禁、无内核沙箱——并且会先问你一次。对专用于一个
智能体的机器是对的，对正在打字的这台笔记本是错的。

在 macOS 上用默认参数实测：`$HOME`、`~/Documents`、`/usr/local/bin`、`/private/var`
写入被拒；`~/.ssh`、`~/Library/Mail`、`~/Library/Messages`、`~/Library/Cookies`
读取被拒；`/tmp` 仍可写，因为禁掉它会让 `pip`、`npm` 以"像 bug 的方式"失败。其余位置只读。

**以 root 运行是最容易被忽略的那一项。** 在 root 下，`/` 和 `./` 是同一个地方，一条坏命令
没有任何边界可撞。现在的默认避开了它：服务以你自己的用户身份运行，用的还是这台机器的工具。


### 两张凭证，不是一张

客户粘贴的那条命令里带的是 **join token（接入凭证）**：一小时有效、绑定一个工作区、只用来换东西。
机器第一次连上时用它换一张**自己的 worker key（节点凭证）**，之后一直用那张——180 天、绑定这台机器的 ID。

| | join token（`jn_…`） | worker key（`wk_…`） |
|---|---|---|
| 有效期 | **1 小时** | **180 天** |
| 交给谁 | 人，粘贴一次 | 机器，以 `0600` 存盘 |
| 出现在 | 安装命令里 | 每次重连的隧道握手里 |
| 吊销 | 按工作区的邀请 | **按机器** |
| 泄露后果 | 一小时内能接入一台机器 | 那一台 worker 的权限 |

**为什么要分开**：能活过重启的凭证就值得偷，而它是发给一个早就忘了它的人的；短到安全的凭证活不过重启，
过期后的第一次重启就会把节点踢出机队——**而且是静默的**，直到有人发现控制台空了。

```bash
# 机器自己保存的那张
$ docker exec sandrpod-worker cat /var/lib/sandrpod/worker-key
{"token":"wk_…","expires_at":"2027-04-27T15:01:40Z"}
```

换取是自动的：有存好的 key 就一直用（命令行里的 token 被忽略）；key 过期会在自己的日志里说明并重新换取；
换不到（老版本控制面、邀请过期）就退回用邀请继续重试——**拒绝启动会把"缺一个功能"变成"一次故障"**。

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
