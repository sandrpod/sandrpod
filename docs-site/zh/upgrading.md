---
title: 升级
description: 顺序、兼容承诺，以及为什么回滚只是改个 tag。
---

# 升级

## 顺序

1. **先升 worker。** 旧 worker 会忽略租户字段；新控制面拒绝把租户沙箱放到它上面 ——
   `409 worker capability unavailable` —— 而不是把两个租户挤在同一台机器上。
2. **再升控制面。** 迁移只做加法；上一个版本仍然能在新 schema 上跑。
3. **最后升 agent。** 它们只调 API。

按这个顺序滚动，没有任何一步需要前一步停机。

## "兼容"是什么意思

| 面 | 承诺 |
|---|---|
| 数据库 schema | 只做加法（`ALTER TABLE … ADD COLUMN`）；不改名、不删除。旧二进制能在新 schema 上跑 |
| HTTP API | 字段只增不删。忽略未知字段的客户端继续工作 |
| Go API（`pkg/server`、`pkg/sandpod`、`pkg/toolbox`） | 遵循 semver；破坏性变更按版本逐条列出 |
| 参数与环境变量 | 同一个大版本内不删除 |
| Python CLI 与 SDK | 在同一大版本内与服务器版本无关 |

回滚唯一无法撤销的是**已经做出的放置决定**。如果一个租户沙箱已经被放到某台 worker 上，
把它挪走是一个显式动作，不是降级的副作用。

## 回滚

把镜像 tag 指回去。因为 schema 只增加列，没有迁移要反向执行。

```bash
docker rm -f sandrpod-cloud
docker run -d --name sandrpod-cloud … sandrpod-cloud:1.2.3   # 上一个 tag
```

## 升级前后

```bash
# 升级前：先知道你在跑什么
sandrpod-cli health
curl -s "$API/metrics" -H "X-Sandrpod-Token: $TOKEN" | grep sandrpod_version

# 升级后：同样两个答案，外加一次真实创建
sandrpod-cli create canary --provider local && sandrpod-cli execute canary "echo ok"
sandrpod-cli delete canary
```

一次创建是唯一能跑通整条链路的健康检查 —— API、调度器、隧道、容器、toolbox ——
所以它属于部署脚本，而不是事后复盘。

## 按版本

每个版本的破坏性变更、影响谁、怎么迁移，记在
[UPSTREAM_CHANGES.md](https://github.com/sandrpod/sandrpod/blob/main/docs/UPSTREAM_CHANGES.md)。
