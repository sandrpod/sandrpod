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

client = SandrPodClient(api_url="http://localhost:8080")

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

## Sandbox surface

| 能力 | 方法 |
|------|------|
| 一次性执行 | `execute(cmd)` |
| 文件 | `ls / read / write / edit / grep / glob / upload_files / download_files` |
| 有状态解释器（变量跨调用保留） | `run_code(code, context_id=…)` + `create/list/restart/remove_code_context` |
| 目录监视 | `watch_dir(path)` / `get_new_events(watcher_id)` |
| 资源用量 | `metrics()` |
| MCP 配置与调用 | `mcp_ls / mcp_add / mcp_rm / mcp_tools / mcp_manifest / mcp_url` |

示例见 [examples/](examples/)（含 `04_personal_mcp.py`：把员工机上的个人 MCP
工具通过沙箱 `/mcp` 端点交给远端 agent）。

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SANDRPOD_API_URL` | API Server 地址 | `http://localhost:8080` |
| `SANDRPOD_API_TOKEN` | 平台层 Bearer token | (empty) |
| `SANDRPOD_MCP_TOKEN` | 资源层 `mcp_token`（沙箱内 bridge 的共享密钥，可选） | (empty) |
