---
title: 第一个沙箱
description: 创建、执行、查看、删除一个沙箱，并把每个环节命名。
---

# 第一个沙箱

一个沙箱就是一个容器，里面跑着 toolbox：一个负责执行代码、维持 PTY、管理
`/workspace` 文件系统的 HTTP 服务。创建它分四步，每一步出问题都能看见。

## 创建

```bash
sandrpod-cli create demo --provider local
```

底层发生的事情：

```
POST /api/v1/sandboxes {"name":"demo","provider_type":"local"}
  → 调度器选一个 worker（地域、厂商、能力、租户）
  → 写入沙箱记录，状态 PENDING
  → 记录一个 job ───────────────────► 这就是"进程中途挂掉也能恢复"的依据
  → 通知 worker 创建容器
  → 容器起来、toolbox 应答、记录变 RUNNING
```

状态流转：`PENDING → STARTING → RUNNING → STOPPING → STOPPED`，另有 `ERROR`
（创建失败）和 `TERMINATED`。

## 执行

```bash
sandrpod-cli execute demo "python3 -c 'print(2**10)'"
sandrpod-cli stream  demo "for i in 1 2 3; do echo \$i; sleep 1; done"
sandrpod-cli shell   demo          # 交互式 PTY，Ctrl-] 退出
```

`execute` 是无状态的：每次调用都是同一文件系统下的新进程。
`stream` 边产生边输出。
`shell` 是真 PTY，提示符、颜色、`Ctrl-C` 都正常。

## 文件

```bash
sandrpod-cli fs mkdir demo /workspace/data
sandrpod-cli fs write demo /workspace/data/hello.txt "从外面写进去的内容"
sandrpod-cli fs ls    demo /workspace/data
sandrpod-cli fs cat   demo /workspace/data/hello.txt
sandrpod-cli fs upload   demo ./local.bin /workspace/local.bin
sandrpod-cli fs download demo /workspace/local.bin ./back.bin
```

## 观察

```bash
sandrpod-cli get   demo      # 状态、所在 worker、IP、架构
sandrpod-cli env   demo      # 沙箱内部的架构、系统、shell
sandrpod-cli stats demo      # 这个沙箱的实时 CPU / 内存 / 磁盘
sandrpod-cli logs  demo --tail 50
```

## 停止、启动、删除

```bash
sandrpod-cli stop demo       # 容器暂停；记录和文件系统保留
sandrpod-cli start demo      # 回到 RUNNING
sandrpod-cli delete demo     # 容器和记录一起删除
```

**停下来的沙箱仍然占用 worker 的容器配额**，直到删除。停止是"暂停"，不是"清理"。

## 快照

```bash
sandrpod-cli snapshot demo --image registry.example.com/tuned-agent:2026-10
```

把容器提交成镜像。这样"装好依赖、clone 好仓库、缓存预热过"的环境就能成为下一个
沙箱的起点。需要 worker 上有 Docker —— local provider 本来就有。

## 下一步

- [出网策略](/zh/egress) —— 决定沙箱能连到哪。
- [Worker 与隧道](/zh/workers) —— 调度器到底选了什么、为什么。
- [命令行 CLI](/zh/cli) —— 完整命令面。
