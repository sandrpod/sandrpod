---
title: 控制台快速开始
description: 托管控制台 —— 注册、拿到工作区、在浏览器里跑起一个沙箱。
---

# 控制台快速开始

[docs.sandrpod.com](https://docs.sandrpod.com) 讲的是**引擎**。这一节讲**托管控制台** ——
带账号、工作区、钱包和浏览器终端的那一套，在
[cloud.sandrpod.com](https://cloud.sandrpod.com)。

底下的引擎就是开源那一套。文档里 [CLI](/zh/cli) 和 [REST API](/zh/api) 的任何东西在
控制台上同样可用；这一节只讲控制台多出来的部分。

## 一、账号与工作区

注册会同时创建两样东西：一个账号，和一个**工作区** —— 沙箱、成员、密钥和钱都装在里面。
大多数人一辈子只需要一个；第二个工作区是用来隔开两个不该共用钱包和审计的项目（或者
两个客户）的。

新工作区从 **trial** 套餐开始，带 **¥5 额度** —— 一个小沙箱能跑几个小时。够你读完这页，
不够你误操作跑起一门生意。

## 二、密钥

所有非浏览器的客户端都需要它。同一份凭据的两种拼写：

| 密钥 | 谁在用 |
|---|---|
| `sk_…` | CLI、`curl`、Python 客户端 |
| `e2b_…` | 未经修改的 E2B SDK（它在客户端校验前缀） |

在 **设置 → API 密钥** 里创建。只显示一次；之后列表里只有前缀。见
[API 密钥](/zh/console-keys)。

## 三、沙箱

门户的 **沙箱 → 新建** 会创建并打开它的页面：

- **终端** —— 浏览器里的真 PTY
- **文件** —— 浏览、上传、下载
- **日志** —— 容器输出
- **指标** —— 这个沙箱的实时 CPU 和内存
- **执行** —— 一次性命令及退出码

## 四、同一件事，用命令行

```bash
pip install sandrpod-cli
export SANDRPOD_API_URL=https://cloud.sandrpod.com
export SANDRPOD_API_TOKEN=sk_...

sandrpod-cli create demo --provider local
sandrpod-cli execute demo "python3 --version"
sandrpod-cli delete demo
```

## 控制台比引擎多了什么

| | |
|---|---|
| 账号 | 邮箱验证、密码重置、可按设备吊销的会话 |
| 工作区 | 两种角色、邀请、切换、删除并留审计 |
| 钱 | 预付钱包、充值、退款、余额不足与欠费提醒 |
| 审计 | 每个生命周期事件，保留 180 天 |
| Worker | join token，让客户接入自己的机器 |
| 支持 | `support@gatemux.ai` |

## 下一步

- [工作区与角色](/zh/console-workspaces)
- [API 密钥](/zh/console-keys)
- [计费与用量](/zh/console-billing)
- [限额](/zh/console-limits)