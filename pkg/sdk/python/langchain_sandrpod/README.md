# langchain-sandrpod

SandrPod sandbox integration for [Deep Agents](https://github.com/langchain-ai/deepagents).

## Installation

```bash
pip install langchain-sandrpod
```

## Quick Start

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent
from deepagents.middleware import FilesystemMiddleware

client = SandrPodClient(api_url="http://localhost:18080")

with client.sandbox("my-sandbox") as sb:
    # 直接执行命令
    result = sb.execute("python3 -c 'print(42)'")
    print(result.output)  # 42

    # 或者接入 deepagents agent
    agent = create_deep_agent(
        middleware=[FilesystemMiddleware(backend=sb)]
    )
    result = agent.invoke({"messages": [{"role": "user", "content": "列出 /workspace 下的文件"}]})
```

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SANDRPOD_API_URL` | API Server 地址 | `http://localhost:18080` |
| `SANDRPOD_API_TOKEN` | Bearer 认证 token | (empty) |
