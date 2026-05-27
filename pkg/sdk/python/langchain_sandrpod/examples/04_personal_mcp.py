"""Use a sandbox's MCP transport bridge as a LangChain tool source.

Prerequisites:
  - sandrpod-agent running on the target machine with ``~/.sandrpod/mcp.json``
    populated (Claude Desktop format works as-is).
  - The sandbox is registered with your SandrPod API Server (direct or
    container mode).
  - ``pip install langchain-mcp-adapters`` for the MCP client used below.

Run::

    SANDRPOD_API_URL=http://localhost:8080 python examples/04_personal_mcp.py my-laptop
"""

from __future__ import annotations

import asyncio
import os
import sys

import httpx

from langchain_sandrpod import SandrPodSandbox


async def main(sandbox_name: str) -> None:
    sb = SandrPodSandbox(sandbox_name=sandbox_name)

    # 1) Sanity-check the bridge: which MCP servers does the agent have up?
    manifest = httpx.get(sb.mcp_manifest_url(), timeout=5.0).json()
    ready = [s for s in manifest["servers"] if s["state"] == "ready"]
    print(f"bridge has {len(ready)} ready server(s), {manifest['total_tools']} tools total")
    for s in ready:
        print(f"  {s['name']}: {s['tool_count']} tool(s)")

    # 2) Hand the URL to any MCP-compatible client.
    try:
        from langchain_mcp_adapters.client import MultiServerMCPClient
    except ImportError:
        print("install langchain-mcp-adapters to use the LangChain side")
        return

    # Two-tier auth (see docs/MCP_BRIDGE.md #Authentication):
    #   - X-Sandrpod-Token authenticates to the API Server
    #   - Authorization: Bearer authenticates to the agent's /mcp endpoint
    # In dev you can omit either by configuring sandrpod-server / -agent
    # without the matching token.
    api_token = os.environ.get("SANDRPOD_API_TOKEN", "")
    mcp_token = os.environ.get("SANDRPOD_MCP_TOKEN", "")
    headers: dict[str, str] = {}
    if api_token:
        headers["X-Sandrpod-Token"] = api_token
    if mcp_token:
        headers["Authorization"] = f"Bearer {mcp_token}"

    client = MultiServerMCPClient(
        {
            "personal": {
                "url": sb.mcp_url(),
                "transport": "streamable_http",
                "headers": headers,
            },
        }
    )
    tools = await client.get_tools()
    print(f"\nloaded {len(tools)} LangChain tools:")
    for t in tools:
        print(f"  - {t.name}")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} <sandbox-name>")
        sys.exit(1)
    asyncio.run(main(sys.argv[1]))
