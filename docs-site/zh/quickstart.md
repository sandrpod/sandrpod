---
title: 快速开始
description: 六十秒跑起控制面和 worker，然后让沙箱执行你的代码。
---

# 快速开始

三种入口，按你手上有什么来选。

## 一、用已发布的镜像（最快）

```bash
curl -O https://raw.githubusercontent.com/sandrpod/sandrpod/main/docker/docker-compose.yml
docker compose up -d
```

| 容器 | 是什么 |
|---|---|
| `server` | 控制面，监听 `localhost:8080` |
| `poder` | Docker worker，通过反向隧道拨回控制面 |
| `toolbox` | 由 worker 在创建沙箱时拉取，一个沙箱一个 |

worker 需要 Docker socket，控制面不需要别的。

验证：

```bash
curl -s localhost:8080/health
# {"mode":"control-plane+tunnel","status":"ok","version":"0.7.0", ...}
```

## 二、建一个沙箱，然后跑代码

```bash
pip install sandrpod-cli

export SANDRPOD_API_URL=http://localhost:8080
export SANDRPOD_API_TOKEN=change-me          # 启动服务端时 -token 给的那个值

sandrpod-cli create demo --provider local
sandrpod-cli execute demo "echo 来自你自己机器上的沙箱"
sandrpod-cli delete demo
```

`create` 默认等到 `RUNNING` 才返回。`--no-wait` 立即返回 job id，适合放进脚本里做
长流程。

## 三、同一件事，用 HTTP

```bash
curl -s -X POST localhost:8080/api/v1/sandboxes \
  -H "X-Sandrpod-Token: change-me" \
  -H 'Content-Type: application/json' \
  -d '{"name":"demo","provider_type":"local"}'

curl -s -X POST "localhost:8080/api/v1/sandboxes/execute?sandbox=demo" \
  -H "X-Sandrpod-Token: change-me" \
  -H 'Content-Type: application/json' \
  -d '{"language":"bash","code":"uname -a"}'
```

## token 从哪来

- 控制面的 `-token`，或环境变量 `SANDRPOD_TOKEN`。
- 它是**运维凭据**：能看到所有沙箱、所有 worker。
- 如果部署要给客户发独立凭据，那是另外签发的（CLI 和原生 API 用 `sk_`，E2B 面用
  `e2b_`）。见 [API 鉴权](/zh/api#鉴权)。

## 下一步

- [第一个沙箱](/zh/first-sandbox) —— 同样的步骤，但把每个部件讲清楚。
- [E2B 直接替换](/zh/e2b) —— 把现有的 E2B 客户端指到你自己的服务器。
- [架构](/zh/architecture) —— 那三个容器到底在干什么。
