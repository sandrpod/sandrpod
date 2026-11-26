---
title: 发布说明
description: 每个版本改了什么，其中哪些改动会破坏东西。
---

# 发布说明

## 0.13.11

修复嵌套目录写文件与快照 fork 拷贝 `/workspace` 卷的问题。

| 领域 | 改了什么 |
|---|---|
| Toolbox 文件上传 | `POST /files/upload`（`sandbox_write_file`）在通过权限校验后自动创建缺失的父目录，不再因父目录不存在报 500 |
| 快照 Fork | `DockerPoder.copyVolume` 直接复用当前已就绪的快照镜像拷贝 `/workspace` 卷，避免 worker 未配 `SANDRPOD_TOOLBOX_IMAGE` 时静默回退为空工作区 |

## 0.13.10

修复平台层 `/mcp`（`sandbox_list`）的 JSON Schema 序列化问题。

| 领域 | 改了什么 |
|---|---|
| 平台层 MCP | `required` 为空时不再输出 `"required": null`（修复严格校验 JSON Schema 的 MCP 客户端禁用 `sandbox_list` 的问题），同时为 `sandbox_list` 补充可选分页参数 `limit` / `offset` |

## 0.13.9

统一平台层 MCP（`/mcp`），通过单一静态入口即可发现并调用任意沙箱内的全部 MCP 工具。

| 领域 | 改了什么 |
|---|---|
| 平台层 MCP | 移除 `/mcp` 上硬编码的 5 个 `desktop_*` 转发工具，改为通用元工具 `sandbox_mcp_list` 和 `sandbox_mcp_call`（总计 15 个正交工具）；配置一个固定 URL 即可统一调用桌面、Obscura 无头浏览器及沙箱 `mcp.json` 里的全部工具 |
| 路由与性能 | 统一经 `/api/v1/sandboxes/{sandbox}/mcp` 转发（同时支持容器与直连 `sandrpod-agent`），按 `(caller, sandbox)` 缓存 `Mcp-Session-Id` 5 分钟并在会话失效时自动重握手重试 |
| 安全边界 | 转发前将平台 Bearer Token 移入会被剥离的 `X-Sandrpod-Token`，仅透传显式指定的 `mcp_token`，确保平台 API Key 绝不泄露到沙箱 |

## 0.13.8

给计量/审计消费者提供可计费的沙箱生命周期事件。

| 领域 | 改了什么 |
|---|---|
| 事件 | 沙箱从 `RUNNING`/`STOPPING` 进入 `STOPPED`/`ERROR` 时发 `sandbox.stopped`，恢复运行时发 `sandbox.started`——覆盖 REST start/stop、E2B pause/resume、空闲回收、心跳对账和任务状态回报五条路径 |
| 载荷 | 与其他沙箱事件相同的 owner / CPU / 内存 / GPU / runtime 字段，另加 `from` |
| 兼容性 | 纯增量：创建仍发 `sandbox.running`，删除仍发自己的事件，忽略未知事件的下游不受影响 |

## 0.13.7

三处安全修复，涉及 worker 权限边界与文件门禁。使用员工机模式（`sandrpod-agent`）
或签发过 API key 的部署建议升级。

| 领域 | 改了什么 |
|---|---|
| 文件搜索 | `files.find` / `files.search` 改为逐路径鉴权，不再只校验根目录：hardlock 下的文件被跳过，符号链接无法越出授权目录，`../` 爬不出去，FIFO 也不会再卡死搜索 |
| API key | 吊销最后一把 key 不再把服务器变成匿名管理员入口：索引一旦持有过 key 就保持鉴权，最后一把 key 会被拒绝（除非配置了静态 token），且拒绝与删除是原子的 |
| 任务 | 删除 `GET /api/v1/jobs/poll`：没有客户端在用，而任何 worker 凭据（包括租户的 provision token）都能借此读到所有租户待创建请求及其环境变量 |

## 0.13.6

AWS provider 再修五处，逐条在真实账号上验过。

| 领域 | 改了什么 |
|---|---|
| 实例状态 | 已终止实例报 `TERMINATED` 而不是 `STOPPED`——等待实例消失的调用方不再看到一台"永远停不掉"的机器 |
| 区域字段 | `GetVM` 的 Region 填请求的区域，而不是可用区 |
| 清理 | `Cleanup` 必须显式设 `SANDRPOD_AWS_ALLOW_CLEANUP=1`：它会终止该区域**所有** SandrPod 实例，不分租户，所以不能靠一次方法调用触发 |
| 机型 | `ListInstanceTypes` 改调 `DescribeInstanceTypes`（`us-east-1` 分页返回 1373 个机型），原静态列表仅作离线兜底 |
| 计费归属 | EBS 卷与实例一起打标签，成本分摊和按标签找孤儿都能看到它 |

## 0.13.5

云 provider 按调度器的假设逐条审计，并在真实账号上各跑了一遍。

| 领域 | 改了什么 |
|---|---|
| AWS | 强制 IMDSv2 且 hop limit 为 1；每次 `RunInstances` 带 `ClientToken`，SDK 重试不会多开一台在计费的机器；公网 IP 缺子网、region 与 `AWS_REGION` 不一致都会被前置拒绝；`SANDRPOD_SPOT_AWS` 真正请求竞价；只有 `Success` 算 SSM 命令成功；删除已终止实例不再报错；实例没跑起来会如实报错而不是返回 pending |
| 腾讯云 | TAT 命令给 15 分钟预算（默认 60 秒，装 Docker + 冷拉镜像必然超过）；没到实例的命令会重试；竞价必须带最高价（`SANDRPOD_SPOT_MAX_PRICE[_TENCENT]`）；删除撞上"终止进行中"会重试；zone 与地域不匹配前置拒绝；NAT 部署的"不要公网 IP"被尊重 |
| 部署 | 大陆网络下钉住 `SANDRPOD_TOOLBOX_IMAGE` / `SANDRPOD_EGRESS_IMAGE` 让调度器预拉——否则首次创建会卡在镜像拉取上超时 |

## 0.13.4

一次权限收窄、一个调度修复，以及竞价容量。

| 领域 | 改了什么 |
|---|---|
| 安全 | worker 凭据只能用隧道握手、任务轮询、自身心跳、邀请换取凭据，以及 agent 直连的两个调用（capabilities、出网决策）；沙箱 API 由中间件按白名单拒绝 |
| 调度 | 异步创建在 VM 启动期间是 `PROVISIONING`，resume pass 不再把几分钟的 provisioning 当作丢投递判死；被放弃的行 30 分钟后回收 |
| 云 | 阿里云竞价实例，开关 `SANDRPOD_SPOT_ALIYUN`（`SpotAsPriceGo`）；默认关闭，按量付费不变 |
| 运维 | `poder -version`；`deploy/prometheus/sandrpod-alerts.yml`，基于 `/metrics` 的五条告警 |

## 0.13.3

为嵌入构建导出 `server.VerifyProvisionToken`。

| 领域 | 改了什么 |
|---|---|
| 嵌入 | 自带 `Config.Authenticator` 的程序不会走引擎内置的 token 校验；现在它可以验证调度器签发的每机凭据，而不是把每个被 provision 的 worker 都拒之门外 |

使用内置认证器的服务行为不变。

## 0.13.2

两个修复：一个收窄了被 provision 的机器能做什么，另一个让「没确认送达的创建」真的可以被重投。

| 领域 | 改了什么 |
|---|---|
| 安全 | 调度器开出来的 VM 拿到的是每机一份的 provision token（worker 角色、绑定请求它的 workspace、用平台 secret 签名），不再是平台 admin token，无法管理整个集群 |
| 可靠性 | outbox 认领改用行里实际存储的时间拼写；由导入、`psql` 或旧版本写下的 job 不会再永远停在 `PENDING` 而日志里一个字都没有 |
| 测试 | `SANDRPOD_RESUME_STALE_AFTER` / `SANDRPOD_RESUME_INTERVAL` 让多实例检查用快节拍跑 resumer，不再和生产节奏赛跑 |

没有 schema 变更。worker 不升级也能用这两个修复。

## 0.13.1

修复控制台使用的 pinned create 路径上的持久化。

| 领域 | 改了什么 |
|---|---|
| 持久化 | 指定 worker 的创建现在和调度路径一样默认 `persist_data`；桌面沙箱 stop/start 后不再丢失 `/workspace` |

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
