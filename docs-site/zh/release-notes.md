---
title: 发布说明
description: 每个版本改了什么，其中哪些改动会破坏东西。
---

# 发布说明

## 0.7.1

修的是只有真实部署才会遇到的路径。

| 领域 | 改了什么 |
|---|---|
| PostgreSQL 放置 | worker 选择用了一个 SQLite 专有的 SQL 函数，所以在 PostgreSQL 上没有任何查询能匹配，调度器报"30 秒内没有 worker 注册"。现在放置正常。 |
| 多实例路由 | 创建可以跨实例（`POST /internal/provision`），不再因为选中的 worker 隧道在对端而失败；隧道归属由心跳续期，转发不再在 worker 连上 90 秒后失效 |
| 投递持久性 | 创建把载荷写进 `jobs.request`，崩溃后任何实例都能恢复；认领是 compare-and-swap，所以恰好一个实例投递 |
| 已删除的沙箱 | 恢复器不再重建记录已被删除的沙箱 |
| E2B envd | envd 主机会路由到网关（之前会落到控制面），并且托管构建自带密钥解析器时也会接受 per-sandbox envd token |
| CLI | `sandrpod-cli` 0.7.1 已发布；`egress`、`session` 和当前的 `fs` 语义都在发布包里 |

## 0.7.0

让引擎可以被嵌入的那个版本。

| 改动 | 破坏性 |
|---|---|
| `pkg/server` 承载 API 服务（`cmd/server` 只剩一个 `main`） | Go API：版本号 ldflags 符号位置变了 |
| `SelectBestFor(region, provider, require)` 多了一个 `tenant` 参数 | Go API：编译错误，一行迁移 |
| `Config.Authenticator` / `Admission` / `Events` | 只增 |
| `RoleWorker`：join token 只能到达两条路由 | 只增 |
| 读不到的资源读数变成 `-1`（原来是 `0.5`） | 行为：乘 100 的仪表盘要处理符号 |
| 沙箱创建从 4.2 秒降到 230 毫秒（镜像已缓存） | — |

逐条变更与每个破坏点的迁移方式：
[UPSTREAM_CHANGES.md](https://github.com/sandrpod/sandrpod/blob/main/docs/UPSTREAM_CHANGES.md)。

## 支持策略

支持最新的 tag 版本。更早的 tag 在 90 天内继续接收安全修复。`main` 是开发线，不是
发布版本。

## 变更记录

完整的变更记录，包括不值得写进发布说明的修复：
[CHANGELOG.md](https://github.com/sandrpod/sandrpod/blob/main/CHANGELOG.md)。
