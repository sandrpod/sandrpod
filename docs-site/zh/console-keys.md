---
title: API 密钥
description: sk_ 与 e2b_ 密钥、worker join token，以及各自能碰到什么。
---

# API 密钥

三种凭据，三件事。

| 凭据 | 格式 | 能到达 |
|---|---|---|
| 工作区密钥（CLI/API） | `sk_<hex>` | 一个工作区：沙箱、文件、执行 |
| 工作区密钥（E2B） | `e2b_<hex>` | 同上，走 E2B 兼容面 |
| Worker join token | `jn_<hex>` | 隧道握手和它自己的心跳 —— 别的都不行 |
| 平台 token | 32 位 hex | 全部、所有工作区（仅运维） |

## 工作区密钥的两种拼写

`sk_` 和 `e2b_` 是同一份凭据换了前缀，因为官方 E2B SDK 在**发请求之前**就校验格式，
不是 `e2b_<hex>` 一律拒绝。

在 **设置 → API 密钥** 创建，选择格式：

```bash
curl -s -X POST https://cloud.sandrpod.com/portal/v1/api-keys \
  -H "Cookie: sandrpod_session=…" -H 'Content-Type: application/json' \
  -d '{"name":"agent-prod","format":"e2b"}'
```

```json
{"key":{"id":"…","name":"agent-prod","prefix":"e2b_1a2b3c","role":"member"},
 "secret":"e2b_1a2b3c…"}
```

密钥只返回**一次**。列表只显示前缀，永不显示密钥本身：存储的是哈希，丢了的密钥只能换，
不能找回。

## 吊销

删除密钥会吊销它的每一处使用，包括 `sk_` / `e2b_` 的另一半 —— 只有一条记录，两种拼写。
被吊销的密钥在所有面上都返回 `401`。

## Worker join token

join token 让你自己的一台机器加入机队。它是刻意弱的：worker 角色只够隧道握手和自己的
心跳，别的都不行 —— 不能列沙箱、不能读文件、不能签发密钥。

在 **Worker → 添加 worker** 里签发；控制台会打印一条带 token 的 `docker run` 命令。
把它当凭据对待（它确实是），机器下线时删掉。

## 平台 token

部署自己使用的运维凭据。控制台不签发它，也绝不应该出现在智能体里。它泄露意味着所有
工作区暴露；要在部署的环境变量里轮换。

## 给智能体什么密钥

| 场景 | 用什么 |
|---|---|
| 一个智能体、一个工作区 | 限定该工作区的 `sk_…`，智能体停用时吊销 |
| 基于 E2B 的智能体 | `e2b_…`，规则相同 |
| 一个工作区里多个智能体 | 每个智能体一把，吊销才能按智能体来 |
| 不该花钱的智能体 | `member` 角色，钱包由工作区 owner 控制 |

## 下一步

- [E2B 直接替换](/zh/e2b) —— `e2b_` 密钥解锁的那个面。
- [限额](/zh/console-limits) —— 一把密钥能创建多少。
