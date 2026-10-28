---
title: Python SDK
description: 用 Python 驱动 SandrPod —— 原生客户端，或者你已经在用的 E2B SDK。
---

# Python SDK

两条路，取决于你的代码现在 import 什么。

## 如果你已经在用 E2B

除了 `e2b` 不需要装任何东西。把它指向你的服务器：

```python
import os
os.environ["E2B_DOMAIN"] = "sandbox.example.com"
os.environ["E2B_API_KEY"] = "e2b_..."

from e2b import Sandbox
sb = Sandbox.create()
print(sb.commands.run("echo hi").stdout)
sb.kill()
```

细节、密钥格式和已验证清单见 [E2B 直接替换](/zh/e2b)。

## 原生客户端

`pip install sandrpod-cli` 同时会装上 CLI 使用的客户端库。

```python
from sandrpod_cli.client import SandrPodClient

client = SandrPodClient(api_url="https://cloud.example.com", token="sk_...")

sb = client.create_sandbox("agent-01", provider="local")
result = client.execute("agent-01", "python3 -c 'print(2**10)'")
print(result["stdout"], result["exit_code"])

client.delete_sandbox("agent-01")
```

或者直接用 HTTP —— 客户端包的就是这套契约：

```python
import requests

API, TOKEN = "https://cloud.example.com", "sk_..."
H = {"X-Sandrpod-Token": TOKEN, "Content-Type": "application/json"}

requests.post(f"{API}/api/v1/sandboxes", headers=H,
              json={"name": "agent-01", "provider_type": "local"}).raise_for_status()

r = requests.post(f"{API}/api/v1/sandboxes/execute?sandbox=agent-01", headers=H,
                  json={"language": "python", "code": "print(2**10)"}).json()
print(r["stdout"])

requests.delete(f"{API}/api/v1/sandboxes/agent-01", headers=H)
```

## 文件与流式输出

```python
client.write_file("agent-01", "/workspace/in.txt", b"hello")
client.read_file("agent-01", "/workspace/in.txt")
for chunk in client.stream("agent-01", "for i in range(3): print(i)"):
    print(chunk, end="")
```

## 长时间运行的工作

沙箱会活到 TTL 到期。对于要跑几个小时的智能体循环，创建一次然后复用；文件系统在
`execute` 之间是保留的，所以即使每次调用都是新进程，状态也可以放在文件里。

```python
sb = client.create_sandbox("work", provider="local", ttl_seconds=4 * 3600)
```

`ttl_seconds` 会回收闲置的沙箱；有活动就重置计时。

## 错误

客户端会带着服务端的原话抛异常。两个值得显式处理：

| 错误 | 含义 |
|---|---|
| `503 sandbox tunnel not available` | 持有这个沙箱的 worker 没连上；等几秒重试，或者重新创建 |
| `403 owner_required` | 凭据是 member 而不是 owner：花钱和授权属于 owner |
