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
    # Run a command directly
    result = sb.execute("python3 -c 'print(42)'")
    print(result.output)  # 42

    # Or plug the sandbox into a deepagents agent
    agent = create_deep_agent(
        middleware=[FilesystemMiddleware(backend=sb)]
    )
    result = agent.invoke({"messages": [{"role": "user", "content": "List the files under /workspace"}]})
```

## Sandbox surface

| Capability | Methods |
|------------|---------|
| One-shot execution | `execute(cmd)` |
| Files | `ls / read / write / edit / delete / grep / glob / upload_files / download_files` (`write` overwrites existing files; `delete` natively calls `/files/delete` — both match the deepagents backend contract) |
| Stateful interpreter (variables persist across calls) | `run_code(code, context_id=…)` + `create/list/restart/remove_code_context` |
| Directory watch | `watch_dir(path)` / `get_new_events(watcher_id)` |
| Resource usage | `metrics()` |
| MCP config & calls | `mcp_ls / mcp_add / mcp_rm / mcp_tools / mcp_manifest / mcp_url` |

See [examples/](examples/) — including `04_personal_mcp.py`, which exposes the
personal MCP tools on an employee machine to a remote agent via the sandbox's
`/mcp` endpoint.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SANDRPOD_API_URL` | API Server address | `http://localhost:8080` |
| `SANDRPOD_API_TOKEN` | Platform-layer Bearer token | (empty) |
| `SANDRPOD_MCP_TOKEN` | Resource-layer `mcp_token` (shared secret of the in-sandbox bridge, optional) | (empty) |
