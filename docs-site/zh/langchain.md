---
title: LangChain 与 deepagents
description: 两行代码，给你的 deepagents 智能体一个自己拥有的沙箱。
---

# LangChain 与 deepagents

`sandrpod` 自带 LangChain 后端，所以 deepagents 智能体可以跑在你自己硬件上的沙箱里，
而不是某个托管服务。

```bash
pip install langchain-sandrpod
```

```python
import os
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent

client = SandrPodClient(api_url=os.environ["SANDRPOD_API_URL"],
                        token=os.environ["SANDRPOD_API_TOKEN"])

sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "画一下 /workspace 里的数据"}]})
```

或者让客户端管理沙箱生命周期：

```python
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    agent.invoke({"messages": [{"role": "user", "content": "ls"}]})
# 退出时沙箱被删除
```

## 环境变量

| 变量 | 含义 |
|---|---|
| `SANDRPOD_API_URL` | 你的控制面 |
| `SANDRPOD_API_TOKEN` | `sk_` 工作区 key，或平台 token |

## 如果你已经在用 `langchain-e2b`

在 deepagents 集成里，两个后端驱动的十二项行为完全一致。只要把 `E2B_DOMAIN` 指向你的
服务器，E2B 那个在 SandrPod 上原样可用 —— 见 [E2B 直接替换](/zh/e2b)。选哪个取决于你
已经依赖哪个包，而不是哪个更强。

## 常见做法

| 想要 | 怎么做 |
|---|---|
| 隔离单个任务 | `with client.sandbox(...)`，退出即删 |
| 复用准备好的环境 | 装好后 `snapshot`，之后从那个镜像创建 |
| 不让智能体上网 | 创建时带 `--allow-domain`，只列它需要的 |
| 保留长对话的文件 | 创建一次，每轮都传同一个沙箱 |
