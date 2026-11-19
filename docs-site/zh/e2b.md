---
title: E2B 直接替换
description: 把未经修改的 E2B SDK 指向你自己的服务器 —— 一个环境变量，零代码改动。
---

# E2B 直接替换

如果你的智能体已经在用 E2B，它可以继续用，只是跑在你自己硬件上。SDK 没有被
修改、没有 fork、没有包一层：给它一个域名，它自己拼 URL。

## 服务端

E2B 兼容面由域名打开：

```bash
# 控制面
SANDRPOD_E2B_DOMAIN=sandbox.example.com      # 兼容面的基础域名
SANDRPOD_E2B_PROVIDER=local                  # 新建 E2B 沙箱落在哪种底座上
SANDRPOD_E2B_REGION=local                    # 必须和你实际运行的 worker 一致
```

设置之后，两类主机开始应答：

| 主机 | 提供什么 |
|---|---|
| `api.sandbox.example.com` | E2B 控制面：`POST /sandboxes`、列表、查询、kill、set-timeout、pause/resume、metadata、快照、fork、网络更新 |
| `<端口>-<sandboxID>.sandbox.example.com` | 沙箱本体：envd RPC（`commands.run`、`files.*`），以及该端口上的任意服务 |

两者都需要 DNS（`api.` 加一个 `*` 泛解析）和证书。反向代理终止 TLS 就够，路径不
需要改写。

## 客户端

```python
import os
os.environ["E2B_DOMAIN"] = "sandbox.example.com"
os.environ["E2B_API_KEY"] = "e2b_..."       # 见下面「密钥」

from e2b import Sandbox

sb = Sandbox.create(timeout=300)
try:
    print(sb.commands.run("uname -s").stdout)
    sb.files.write("/tmp/notes.txt", "从 SDK 写进去的")
    print(sb.files.read("/tmp/notes.txt"))
finally:
    sb.kill()
```

这就是全部集成工作。SDK 自己的校验、重试、流式输出，行为和连托管服务时完全一致。

## 密钥

E2B SDK 在**发送请求之前**就在客户端校验密钥：必须是 `e2b_<hex>`，别的拼写直接抛
`AuthenticationException`。

托管部署发放的是同一份凭据的两种拼写：

| 拼写 | 谁在用 |
|---|---|
| `sk_<hex>` | CLI、原生 REST API |
| `e2b_<hex>` | E2B SDK（同一张表、同样吊销、同一个工作区） |

商业控制台在 **Settings → API keys** 里签发，`format` 字段决定拼写。自托管的单租户
服务器上，平台 token 两种都能用。

## 已验证的内容

在真实部署上用官方 `e2b` 包、零补丁跑过。2026-11-19 在 **v0.12.1** 上复跑：
`e2b` 2.64.0 **38/38**；2.30.0 为 37/38，唯一缺口是该版本还没有
`Sandbox.fork`。

| 调用 | 结果 |
|---|---|
| `Sandbox.create()` | 返回 sandbox id 和 `envdAccessToken` |
| `sb.commands.run("echo …")` | 返回 stdout 和退出码，支持流式与后台 |
| `sb.files.write` / `sb.files.read` | 读写往返、批量写、签名上传/下载 URL |
| `sb.pty.*` | 交互式 PTY、resize、kill |
| `sb.kill()` | 终止 |
| `sb.create_snapshot()` / `Sandbox.list_snapshots()` | 快照 ID（`repo:tag`）、列表、删除 |
| `sb.fork()` | 从源沙箱快照启动 N 个子沙箱 |
| `sb.update_network()` | 替换运行中沙箱的 egress sidecar |
| `create(network={"allow_public_traffic": False})` | 该沙箱所有公共端口都要求凭证 |
| `GET <端口>-<id>.…/health` | envd 健康 |
| `POST /filesystem.Filesystem/Stat` | 目录项 |

这个面上的归属校验和原生 API 完全一致：A 工作区建的沙箱，B 工作区看不到；放置也
被限制在该工作区的 worker 上。

## 两个最容易踩的坑

1. **只按路径挂载**。挂 `/sandboxes` 足以支持 create 和 kill，但**不足以支持
   `commands.run`**：envd 在 `<端口>-<id>.<域名>` 上，它的路径（`/health`、
   `/process`）和原生 API 重名。如果主机没有路由，请求会落到控制面，而控制面会
   返回自己的健康文档、状态码 200 —— 沙箱看起来活着，每条命令都发错了地方。
2. **地域不匹配**。E2B 默认底座地域是 `local`。注册在别的地域的 worker 对它不可见，
   创建会报 `no available local poder found`，而机队其实一切正常。

## 兼容性说明

- `templateID` 如果长得像容器镜像（`repo/image:tag`），直接当镜像用；其它值回落到
  部署的默认镜像。
- 设置 `SANDRPOD_E2B_DOMAIN` 后会提供代码解释器面（`/execute`、`/contexts`）；有状态
  上下文需要 worker 上启用 Jupyter 那一侧。
- `Sandbox.create(envs={...})` 会写到沙箱容器环境里，命令、PTY 和代码解释器内核都会
  继承；单次命令的 `envs=` 仍然只覆盖那一次调用。
- `create_snapshot`、`list_snapshots`、`delete_snapshot` 映射到 worker 的快照镜像。
  快照 ID 就是 Docker 的 `repo:tag`；删除时本地 tag 和 registry 副本（如果存在）一起清。
- `fork()` 先给源沙箱做快照，再把子沙箱放在**持有该镜像的 worker** 上。E2B 按子项
  返回成功/失败，一个子项被拒不会掩盖其它子项的结果。
- `update_network()` 替换运行中沙箱的 egress sidecar。给从未有过策略的沙箱新增策略会
  返回 `409`：Docker 不能把运行中的容器换到另一张网络。
- `cpu_count` 在 E2B 创建时生效（默认 2 vCPU）；`get_info`、list 和 `get_metrics`
  都报告记录里存的值。
- `download_url`/`upload_url` 带 SDK 生成的签名 URL，不需要额外请求头即可使用，与
  E2B 一致。
- 普通服务的 `get_host(port)` 默认无凭证可访问（不可猜的主机名就是能力）。toolbox
  控制端口 **8080 永不代理**。桌面（5900/6080/6901）和调试器（9222/9223）端口例外：
  仍需控制台签发的签名 URL 或 API key，因为它们暴露的是整台可交互机器。
  `create(network={"allow_public_traffic": False})` 让**这个沙箱**的所有端口都要求
  凭证；部署级的 `SANDRPOD_E2B_PRIVATE_PORTS=1` 对所有沙箱做同样的事。envd、
  代码解释器和签名文件 URL 两种情况都照常鉴权。匿名公共端口流量按客户端 IP + 沙箱
  限速（默认 50 请求/秒，突发 100）；`SANDRPOD_E2B_ANON_RATE` 可调整速率，`0` 关闭。
- `autoPause` 会被接受：闲置超时后停止沙箱并保留 `/workspace`。进程内存不会被保留，
  因此 `autoPauseMemory=true` 会明确拒绝，而不是悄悄降级。`diskSizeMB` 报告为 2048；
  toolbox 不可用时 `get_metrics` 回退到这个声明值。
- 逐端点的状态表在
  [E2B_COMPAT.md](https://github.com/sandrpod/sandrpod/blob/main/docs/E2B_COMPAT.md)。

## 下一步

- [REST API](/zh/api) —— 原生面，做同样的事，用 SandrPod 的词汇。
- [Worker 与隧道](/zh/workers) —— 这些沙箱到底跑在哪。
