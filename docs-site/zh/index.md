---
title: SandrPod 是什么
description: 跑在你自己机器上的 AI 智能体执行基础设施 —— 任何云、任何机器，或者干脆不用云。
---

# 给 AI 智能体用的执行基础设施

SandrPod 把你已经在付费的机器，变成按需创建、用完即弃的沙箱。它由三部分组成：
一个控制面、一个 worker、一个跑在沙箱里的执行引擎 —— 你可以把三个进程塞进一台
笔记本，也可以铺到八家云厂商的机队上。

智能体感知不到区别。它用的 SDK 也感知不到。

<div class="pillars">

**跑在你拥有的任何地方**
: 八家云厂商、普通 Docker，或者一台**连 Docker 都没装**的裸机（用
  `sandrpod-agent`）。worker 不需要任何入站端口：它们主动拨出去。

**说任何 SDK**
: 原生 REST、Python、TypeScript、LangChain / deepagents，以及**未经修改的 E2B
  SDK** —— 直接替换。改一个环境变量，不改你的代码。

**控制权在你手上**
: 数据自托管、可选的权限闸门、决策审计、按沙箱的出网白名单、沙箱看不见明文的
  凭据遮蔽。

</div>

## 六十秒

```bash
curl -O https://raw.githubusercontent.com/sandrpod/sandrpod/main/docker/docker-compose.yml
docker compose up -d
```

这条命令会拉取三个已发布镜像，在 `localhost:8080` 启动控制面，并启动一个通过反向
隧道拨回控制面的 Docker worker。不需要 Go 工具链、不需要构建、不需要 clone。

```bash
pip install sandrpod-cli
sandrpod-cli create demo --provider local
sandrpod-cli execute demo "echo 来自你自己机器上的沙箱"
```

## 你该从哪一页开始

| 你想做什么 | 去看 |
|---|---|
| 五分钟内跑起一个沙箱 | [快速开始](/zh/quickstart) |
| 继续用现有的 E2B 代码 | [E2B 直接替换](/zh/e2b) |
| 从脚本或智能体里驱动它 | [CLI](/zh/cli) · [REST API](/zh/api) |
| 搞清楚各个部件在干什么 | [架构](/zh/architecture) |
| 放到真实流量前面 | [生产部署](/zh/deploy) |
| 把它当成付费服务来运营 | 引擎就是这一套，商业控制台在同一引擎上构建 |

## 它不是什么

SandrPod 不是那种注册就能用的托管产品。引擎是 Apache 2.0 的，完全跑在你控制的
基础设施上。我们在 [cloud.sandrpod.com](https://cloud.sandrpod.com) 有一套基于同一
引擎的商业控制台，但这份文档里的任何内容都不依赖它。
