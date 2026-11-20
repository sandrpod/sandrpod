---
title: 命令行 CLI
description: sandrpod-cli 参考 —— 每条命令都在真实部署上验证过。
---

# 命令行 CLI

```bash
pip install sandrpod-cli          # 或：uv tool install sandrpod-cli
sandrpod-cli --version            # 0.7.3
```

```bash
export SANDRPOD_API_URL=https://cloud.example.com
export SANDRPOD_API_TOKEN=sk_...          # 单租户服务器上也可以用平台 token
```

下面每一条命令都在引擎 0.13.1、CLI 0.7.3 的部署上实际跑过。

## 配置

| 命令 | 作用 |
|---|---|
| `config view` | 显示 API URL、掩码后的 token 和配置文件路径 |
| `config set-url <url>` | 保存 API URL |
| `config set-token <token>` | 保存 token（文件权限 `0600`） |
| `config unset-token` | 移除已保存的 token |

`--api-url` 和环境变量优先于配置文件（`SANDRPOD_API_URL`、
`SANDRPOD_API_TOKEN`）。CLI 没有 `--token` 参数：token 只从环境或配置文件读，
不进命令行历史。

## 两个名字

每个沙箱有一个**显示名**（`web-dev`，你起的、工作区里人认得的标签）和一个
**routing id**（`sb-0123456789abcdef`，全局且不可变）。`create` 接收显示名；
`list`/`get` 同时打印两者。所有命令都接受任一形式，但 CLI 会先解析，实际请求
一律带 routing id。脚本和书签请用 routing id：预览主机、API 路径和 E2B 表面
都用它寻址。

## 沙箱

| 命令 | 作用 |
|---|---|
| `create <显示名> [--provider p] [--region r] [--instance-type t] [--image i]` | 创建并等待 `RUNNING`；打印 routing id |
| `create <名字> --no-wait` | 立即返回 job id，用 `job get` 轮询 |
| `create <名字> --poder <id>` | 跳过调度器，放到指定 worker |
| `create <名字> --ttl 3600 --cpu 2 --memory 2048` | 闲置 TTL、核数、内存（MiB） |
| `create <名字> --wait-timeout 300` | 等待 `RUNNING` 的秒数（默认 900） |
| `create <名字> --runtime gvisor` | 以 gVisor（`runsc`）运行；需要支持该能力的 worker |
| `create <名字> --allow-domain a.example --deny-domain b.example` | 创建时带出网白名单 |
| `list` / `get <名字>` / `delete <名字…>` | 清点；`list`/`get` 同时显示两个名字 |
| `start <名字>` / `stop <名字>` | 恢复 / 暂停 |
| `env <名字>` | 沙箱内部的架构、系统、shell |
| `stats <名字>` | 该沙箱的实时 CPU、内存、磁盘 |
| `logs <名字> [--tail N]` | 容器日志 |

## 代码

| 命令 | 作用 |
|---|---|
| `execute <名字> "<代码>"` | 一次性执行，无状态 |
| `stream <名字> "<代码>"` | 同上，边执行边输出 |
| `shell <名字>` | 通过 WebSocket 的交互式 PTY（`Ctrl-]` 退出） |
| `snapshot <名字> [--image repo:tag] [--timeout 600]` | 把容器提交成镜像 |
| `preview <名字> <端口> [路径]` | 访问沙箱内某个端口上的服务 |

## 文件

| 命令 | 说明 |
|---|---|
| `fs ls\|cat\|info\|mkdir\|rm` | 路径是沙箱内部的路径 |
| `fs write <名字> <路径> <内容>` | 内容是一个参数，不是本地文件 |
| `fs upload <名字> <本地> <远端>` / `fs download <名字> <远端> <本地>` | 二进制安全 |
| `fs mv <名字> <源> <目标>` / `fs replace <名字> <路径> <旧> <新>` | |
| `fs search <名字> <模式> [--path 路径]` / `fs grep <名字> <模式> [--path 路径]` | 模式是位置参数；`--path` 缩小搜索范围 |
| `fs watch <名字> <路径> [--recursive] [--interval 1]` | 实时打印文件事件，`Ctrl-C` 结束 |

## 出网

| 命令 | 作用 |
|---|---|
| `egress get <名字>` | 当前白名单；未配置时为 `null` |
| `egress set <名字> <文件\|-\|JSON>` | 替换策略 |
| `egress clear <名字>` | 移除策略（会要求确认；脚本里加 `-y`） |

策略在**容器创建时**生效。对运行中的沙箱设置，只是存下来给下次创建用 —— 见
[出网策略](/zh/egress)。

## MCP

沙箱自带的原生 MCP bridge（`/mcp`）会聚合 `/workspace/.sandrpod/mcp.json` 里的
服务器，并在文件变化时热加载。

| 命令 | 作用 |
|---|---|
| `mcp ls <名字>` | 查看沙箱 `mcp.json` 里的服务器 |
| `mcp add <名字> <服务器> <命令>` | 添加 stdio 服务器 |
| `mcp add <名字> <服务器> --url https://… [--type sse]` | 添加远程服务器 |
| `mcp add … [--env K=V] [--header K=V]` | 环境变量或请求头（可重复） |
| `mcp rm <名字> <服务器>` | 移除服务器 |
| `mcp url <名字>` | 打印 bridge URL 和连接所需的两个鉴权头 |
| `mcp tools <名字>` | 查看聚合后的服务器与工具数量 |

`--config-path` 可覆盖 `ls`/`add`/`rm` 自动探测的 `mcp.json` 路径。

## 有状态执行

| 命令 | 作用 |
|---|---|
| `run <名字> "<代码>" --context <id>` | 在有状态内核里执行，变量跨调用保留 |
| `context create\|list\|restart\|rm <名字>` | 管理内核 |
| `session create <名字> [--session-id id]` | 启动长驻 shell 会话 |
| `session list <名字>` / `session get <名字> <id>` | 清点与查看 |
| `session exec <名字> <id> "<命令>"` | 在该会话里执行命令 |
| `session delete <名字> <id>` | 结束会话 |

上下文依赖代码解释器通路；没有启用的部署上，`run` 退化为无状态执行。

## 任务与历史

| 命令 | 作用 |
|---|---|
| `job get <job-id>` | 异步创建的状态、错误、结果 |

## 运维命令

这些需要平台 token，不是工作区 key。

| 命令 | 作用 |
|---|---|
| `poder list\|get\|delete` | worker 机队；`delete --keep-vm` 保留云主机 |
| `metrics` | 控制面的 Prometheus 指标 |
| `token create <名字> [--role user\|admin]` | 签发引擎 token（`e2b_<hex>`），只显示一次 |
| `token list` / `token rm <前缀>` | 清点与吊销 |
| `health` | 控制面自己的健康应答 |

## 退出码与错误

`0` 成功，非零失败并在 stderr 打印服务端的原话。值得记住的两个：

```
Error: Error (HTTP 503): sandbox tunnel not available
```

沙箱存在，但它的 worker 没有连在当前实例上 —— 见[故障排查](/zh/troubleshooting)。

```
Error: Error (HTTP 409): worker capability unavailable
```

请求需要某个能力（比如会执行出网策略的 worker），而没有可用 worker 具备。拒绝是
故意的：另一种做法是给你一个"号称被墙住、其实没有"的沙箱。
