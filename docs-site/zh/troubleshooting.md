---
title: 故障排查
description: 这套系统实际会产生的故障，以及每一个意味着什么。
---

# 故障排查

下面每一条都是真实发生过的故障及其原因，按"最容易被误判成别的东西"排序。

## `no available local poder found`

创建找不到 worker。消息里说 `local` 是因为 provider 是 local，不是因为机队坏了。

| 原因 | 怎么查 |
|---|---|
| 没有任何 worker 注册过 | `sandrpod-cli poder list` |
| **地域不一致** —— 客户端默认 `local`，worker 注册成 `hk` | `select id, region from poders`，两边必须一致 |
| worker 到了容器上限 | 看 worker 行的 `usage_containers` / `max_containers`；删掉已停止的沙箱 |
| 重启后 worker 离线 | 心跳停了；看 `docker logs sandrpod-worker` |
| 租户不匹配 | 为一个工作区签的 join token 不能承载另一个工作区的沙箱 |

最后一条是故意的：拒绝，好过把两个租户挤在一起。

## `sandbox tunnel not available`（503）

沙箱存在，但持有它隧道的 worker 没有连在收到请求的那个实例上。

- 单实例：worker 离线，或者隧道断了正在重连（它每几秒重试一次）。
- 多实例：owner 记录过期了。检查持有方实例的 `-node-url` 非空且在收心跳；
  `tunnel_owners.updated_at` 应该是几秒前，而不是几分钟前。

## `worker capability unavailable`（409）

请求需要的能力没有任何可用 worker 具备 —— 出网策略需要带 `egress` 能力的 worker。
部署这样的 worker，或者去掉策略。

## 沙箱卡在 `PENDING`

创建没有到达 worker。job 行会说明原因：

```bash
sandrpod-cli job get <job-id>
```

`no available local poder found` → 见上。什么都没有 → 进程死在投递中途；恢复器会在
大约一分钟内接过去，而一条超过这个时间又没有错误的 job 值得查一下。

## `create egress proxy container: No such image`

worker 上没有 `ghcr.io/sandrpod/egress-proxy`。拉取之后重建 —— 失败的那次已经写入了
记录，所以先把它删掉。

## 删掉的沙箱又回来了

删除会清掉记录和容器；一条仍然 `PENDING` 的创建 job 曾被恢复器重新投递。0.7.1 已修：
沙箱记录不存在、或已处于终态的 job 会判失败而不是重放。旧版本上如果遇到，升级。

## 用 E2B SDK 时 `commands.run` 返回 401

SDK 不会把 API key 发给 envd。它把创建响应里的 `envdAccessToken` 作为 `X-Access-Token`
发出去，而托管部署必须接受这个 token，不能只认自己的工作区 key。0.7.1 已修；更早的
版本上只有 `create` 和 `kill` 能用。

## E2B SDK 在发请求之前就拒绝密钥

`Invalid API key format: expected "e2b_" followed by hex characters`。密钥必须用这种
拼写签发 —— 同一张表、同样吊销、只是前缀不同。见 [E2B 直接替换](/zh/e2b#密钥)。

## envd 主机返回了 200，但健康文档不是 envd 的

主机没有路由到 E2B 面。只按路径挂载（`/sandboxes`）足以支持创建和删除，不足以支持
`<端口>-<id>.<域名>`；请求落到了控制面。托管构建在 0.7.1 已修；检查你的反向代理是否
原样传递 `Host` 头。

## 自签部署上 TLS 失败

证书通常只签了一个名字。用 IP 连接、而证书上是主机名时，即使 CA 被信任也会验证失败。
用证书上的名字连接，或者把 CA 加进进程的信任库（`SSL_CERT_FILE`）。

## 整体很慢，或者主机干脆不响应

先看内存，再看 CPU。同一台机器上的 `go build` 拖垮过整台主机；同一台机器上既跑门户又
跑一批沙箱也一样。

```bash
free -h; uptime; ps aux --sort=-%mem | head
```

解决办法几乎总是"别再合租"：worker 单独机器、控制面小、数据库托管。见
[生产部署](/zh/deploy)。

## 该看哪里

| 问题 | 命令 |
|---|---|
| 平台健康吗 | `curl -s https://…/healthz \| jq` |
| 机队在干什么 | `sandrpod-cli poder list` |
| 这次创建做了什么 | `sandrpod-cli job get <job-id>` |
| 容器在干什么 | `sandrpod-cli logs <名字> --tail 100` |
| worker 在干什么 | `docker logs sandrpod-worker` |
