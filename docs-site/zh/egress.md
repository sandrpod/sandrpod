---
title: 出网策略
description: 沙箱绕不过去的白名单 —— 怎么表达、怎么执行、什么时候生效。
---

# 出网策略

默认情况下，沙箱拥有其容器的网络能力。出网策略把它收窄到一份名单，而在 Docker
worker 上，这种收窄是结构性的，不是劝告性的。

## 策略

```json
{
  "allowed_domains": ["pypi.org", "github.com", "*.example.com"]
}
```

创建时带上，或者事后替换：

```bash
sandrpod-cli create agent-01 --provider local --allow-domain pypi.org --allow-domain github.com
sandrpod-cli egress get agent-01
sandrpod-cli egress set agent-01 policy.json
sandrpod-cli egress clear agent-01
```

`egress clear` 是移除策略（回到"未配置"），**不等于**设置 `{}` —— 后者是什么都不允许。

## 在 Docker worker 上如何执行

```
沙箱容器 ──► 内部 docker 网络（无出口）
                  │
                  ▼
          egress-proxy sidecar ──► 互联网
          （同时挂在普通网络上）
```

容器只挂在内部网络上，所以它唯一的路就是那个代理，代理按域名放行或拒绝。绕过不了：
直接连 IP 也没有用，因为没有可连出去的路径。

两个后果：

- worker 必须在**第一个这类沙箱之前**就拥有
  `ghcr.io/sandrpod/egress-proxy` 镜像；否则创建会在记录写入之后失败。
- 只有宣告了 `egress` 能力的 worker 能承载这些沙箱。机队里没有的话，创建返回
  `409 worker capability unavailable`，而不是启动一个没有执行的沙箱。

## 什么时候生效

**在容器创建时。** 网络是和容器一起建的，所以对运行中的沙箱设置策略，只是存下来等到
这个沙箱下次被创建时生效。它不是实时防火墙。

这是故意的 —— 就地重建一个容器的网络，本质上是一次重启加上额外的步骤 —— 但也意味着
`create --allow-domain` 才是真正重要的路径，`PUT /egress` 服务于这个沙箱的下一世代。

## 验证

```bash
sandrpod-cli create box --provider local --allow-domain example.com
sandrpod-cli execute box "curl -s -o /dev/null -w '%{http_code}' https://example.com"   # 200
sandrpod-cli execute box "curl -s -o /dev/null -w '%{http_code}' https://pypi.org"      # 000
```

`000` 是 curl 的"连不上"：连接根本没到网络，所以也没有谁来应答。

## Agent 模式

在真实机器上（`sandrpod-agent`），执行方式不同，因为没有网络命名空间可以摘掉：

| 模式 | 行为 |
|---|---|
| `off` | 没有策略 |
| `enforce` | 读本地策略文件并通过代理 |
| `server` | 启动即拒绝一切，轮询控制面拿策略 |
| `decider` | 每个连接向控制面要一次裁决，失败即拒绝，缓存 10 秒 |

代理监听在回环地址上，子进程拿到它的环境变量，所以沙箱进程的流量会经过它。拒绝会以
模型能读懂的原因记进审计。

## Decider 模式

`POST /api/v1/policy/decide` 一次裁决一个目标：

```json
{"sandbox":"agent-01","host":"pypi.org","port":443}
```

```json
{"allow":true,"rule":"allowed_domains","reason":"matched pypi.org"}
```

拒绝是 `200` 加 `allow:false`，不是 HTTP 错误：智能体必须能区分"被拒绝"和"裁决服务
挂了"，而后者必须按拒绝处理。

## 凭据遮蔽是另一件事

白名单决定流量**去哪**；凭据遮蔽决定沙箱在流量**里能看到什么**：沙箱里的哨兵值在代理
处被替换成真凭据，TLS 用每主机证书终止，AWS SigV4 请求被重新签名。沙箱手里从来没有
那个密钥。见
[CREDENTIALS.md](https://github.com/sandrpod/sandrpod/blob/main/docs/CREDENTIALS.md)。
