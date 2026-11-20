---
title: 发布说明
description: 每个版本改了什么，其中哪些改动会破坏东西。
---

# 发布说明

## 0.13.0

第一个支持 **可选 gVisor 运行时** 的版本。

| 领域 | 改了什么 |
|---|---|
| 运行时 | `runtime: "gvisor"`（CLI `--runtime gvisor`）让沙箱以 `runsc` 运行；worker 宣告 `gvisor` 能力，没有可用节点时创建返回 `409` |
| 节点安装 | `deploy/node-install.sh` 默认安装并注册 gVisor；`--no-gvisor` 可关闭 |
| 网络 | gVisor 沙箱用 `SANDRPOD_GVISOR_DNS` 获得上游 resolver；egress sidecar 写入 `/etc/hosts`，实时网络更新后同步更新 |
| 限制 | gVisor 下快照/分叉返回 `409`（`docker commit` 不捕获 runsc 写入）；GPU + gVisor 返回 `400` |
| CLI | `sandrpod-cli 0.7.3` 增加 `--runtime` |

## 0.12.1

修复 0.12.0 新增的 E2B 快照与 fork 路径。

| 领域 | 改了什么 |
|---|---|
| E2B fork | 子沙箱创建在持有源快照的 worker 上，不再被调度到没有该镜像的节点 |
| E2B 快照 | `create_snapshot` 返回的 ID 就是 Docker 实际存储的 `repo:tag`，`list_snapshots` / `delete_snapshot` 看到同一个值 |
| 放置 | 指定 worker 的创建与正常调度使用同一套租户规则；平台持有的沙箱可以 fork 回源 worker |

## 0.12.0

E2B 表面补到可以无保留地跑官方 SDK，同工作区沙箱也不再共享网络。

| 领域 | 改了什么 |
|---|---|
| E2B 快照 | `create_snapshot` / `list_snapshots` / `delete_snapshot` 映射到 worker 快照镜像；删除时同时清本地 tag 和 registry 副本 |
| E2B fork | `fork` 先给源沙箱做快照，再启动请求的数量，逐项返回成功/失败 |
| E2B 网络更新 | `PUT /sandboxes/{id}/network` 替换运行中沙箱的 egress sidecar；给从未有过策略的沙箱新增策略会明确拒绝，因为 Docker 不能给运行中的容器换网络 |
| 私有端口 | `allowPublicTraffic=false` 让所有公共沙箱端口都要求签名 URL 或 API key |
| 隔离 | 每个非 egress 沙箱独立网桥（`sandrpod-s-<hash>`）；worker 自己接入，删除沙箱时一并清理 |
| 限额 | CPU/内存/存储有安全默认值，不再以 0 传给 Docker 变成"无限"；无主历史记录只对 admin 可见 |
| 出网 | egress sidecar 镜像缺失时会按需拉取，而不是让策略创建失败 |
| CLI | `sandrpod-cli` 0.7.2 独立发布（snapshot `--timeout`、`fs grep` 响应处理、0600 配置） |

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
