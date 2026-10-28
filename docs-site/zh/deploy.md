---
title: 生产部署
description: 真实流量压上来时要照抄的部署形态。
---

# 生产部署

一个控制面、PostgreSQL、一个真实域名、TLS、备份、告警。下面每一条都实际跑过，数字
来自真实部署。

## 需要什么

| 部件 | 够用的规格 |
|---|---|
| 控制面 | 2 vCPU / 4 GB —— 进程空转约 60 MB |
| 数据库 | PostgreSQL 14+，开启 TLS；托管版值得，因为备份和 PITR 不用自己写 |
| worker | 每台 4 vCPU / 8 GB / 100 GB；吃负载的是沙箱，不是控制面 |
| TLS | 能自动签发证书的反向代理（Caddy 直接能做） |
| DNS | 一个给 API/控制台的名字，可选一个泛解析给 E2B 面 |

## 形态

```
                ┌──────────────┐
   互联网 ─────►│  TLS 代理     │  Caddy / nginx / 云负载均衡
                └──────┬───────┘
                       ▼ 127.0.0.1:8090
                ┌──────────────┐
                │   控制面      │
                └──────┬───────┘
                       ▼ PostgreSQL（TLS）
                ┌──────────────┐        ┌──────────────┐
                │   worker     │  …     │   worker     │
                └──────────────┘        └──────────────┘
```

把控制面绑在回环地址上，让代理独占公网端口。其它什么都不需要可达。

## 必须配对的环境变量

```bash
SANDRPOD_TOKEN=…                  # 运维凭据，32 位 hex
SANDRPOD_PUBLIC_URL=https://cloud.example.com
CLOUD_DB=postgres://user:pass@host:5432/db?sslmode=require
CLOUD_REGISTRATION=open           # 或 invite / closed
CLOUD_BOOTSTRAP_ADMIN=admin@example.com
CLOUD_BOOTSTRAP_PASSWORD=…        # 只在还没有任何账号时生效
CLOUD_SMTP_HOST=…                 # 不填则邮件打到日志
```

`SANDRPOD_PUBLIC_URL` 不是装饰：邮件链接和支付回跳地址都从它拼出来，填错会在最糟的
时刻失败（客户点"重置密码"的时候）。

## 第一个客户之前要证明的三件事

1. **备份能恢复。** 没恢复过的备份只是假设。
   ```bash
   pg_dump "$DSN" -Fc -f /backup/drill.dump
   psql "$DSN" -c "CREATE DATABASE drill"
   pg_restore -d "${DSN%/*}/drill" --no-owner /backup/drill.dump
   # 用备份起第二个实例，用生产密码登录，比对钱包/审计数字，然后删掉那个库
   ```
2. **告警真的会响。** 故意把服务停掉，确认邮件到了 —— 日志里写着"已发送"不是同一个
   事实。
3. **`/healthz` 说 ok**，并且有 worker 在线：
   ```bash
   curl -s https://cloud.example.com/healthz | jq
   ```

## 不能省掉的维护

| 任务 | 频率 |
|---|---|
| 数据库导出并校验 | 每晚（`pg_dump -Fc`，然后 `pg_restore -l`） |
| 恢复演练 | 每季度，以及任何 schema 变更之后 |
| 清理 worker 上的镜像和容器 | 每周 —— 磁盘是最先被吃满的资源 |
| 依赖与基础镜像更新 | 每月 |
| 证书续期 | Caddy 或 ACME 自动；每季度检查一次 |

## 相关的下一页

- [多实例](/zh/multi-instance) —— 当一个控制面不够用。
- [升级](/zh/upgrading) —— 顺序，以及为什么回滚只是改 tag。
- [故障排查](/zh/troubleshooting) —— 这个形态实际会产生的故障。

带每条命令和输出的参考部署在
[PRODUCTION_DEPLOYMENT.md](https://github.com/sandrpod/sandrpod/blob/main/docs/PRODUCTION_DEPLOYMENT.md)。
