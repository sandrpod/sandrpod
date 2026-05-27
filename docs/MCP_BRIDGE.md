# SandrPod MCP Transport Bridge — User Guide

> **Status**: Beta (v0.1). Local-only deployments are stable; remote-via-tunnel
> deployments need the API Server upgrade described in §SERVER.

## What it does

The MCP Transport Bridge lets you put stdio MCP servers (the kind Claude
Desktop, Cursor and Cline run) on your laptop and have a **remote** AI
orchestrator use them as if they were standard HTTP MCP servers. Your
GitHub PAT, your Notion token, your local filesystem MCP — they stay on
your machine; only the tool calls cross the network.

Three deployment modes:

| Mode | When | Listens on |
|---|---|---|
| `--mcp-only` | Local LAN, dev box, drop-in replacement for `mcp-proxy` | TCP `127.0.0.1:7090` |
| As part of `sandrpod-agent` | Employee PC with a remote API Server | yamux tunnel — exposed at `/api/v1/sandboxes/{name}/mcp` |
| Embedded in your own Go binary | Custom integration | Whatever you wire `mcpbridge.NewHTTPHandler` to |

## Quick start

### 1. Copy your Claude Desktop config

```bash
# macOS
cp ~/Library/Application\ Support/Claude/claude_desktop_config.json \
   ~/.sandrpod/mcp.json
chmod 600 ~/.sandrpod/mcp.json

# Linux (XDG)
cp ~/.config/Claude/claude_desktop_config.json ~/.sandrpod/mcp.json

# Windows (PowerShell)
copy "$env:APPDATA\Claude\claude_desktop_config.json" "$env:APPDATA\sandrpod\mcp.json"
```

The format is byte-identical — every existing `mcpServers` block works.

### 2. Run in standalone mode

```bash
sandrpod-agent --mcp-only --mcp-listen 127.0.0.1:7090
```

Verify:

```bash
curl http://127.0.0.1:7090/mcp/manifest | jq
```

You should see every server in your `mcp.json` listed with `state: "ready"`
and a tool count.

### 3. Point a client at it

Any MCP-compatible client works. Example with the official Python SDK:

```python
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client

async with streamablehttp_client("http://127.0.0.1:7090/mcp") as (r, w, _):
    async with ClientSession(r, w) as session:
        await session.initialize()
        tools = await session.list_tools()
        print([t.name for t in tools.tools])  # ['github__list_issues', ...]
```

LangChain via [`langchain-mcp-adapters`](https://github.com/langchain-ai/langchain-mcp-adapters):

```python
from langchain_mcp_adapters.client import MultiServerMCPClient

client = MultiServerMCPClient({
    "personal": {"url": "http://127.0.0.1:7090/mcp", "transport": "streamable_http"},
})
tools = await client.get_tools()
```

See `pkg/sdk/python/langchain_sandrpod/examples/04_personal_mcp.py` for a
ready-to-run example using the SandrPod Python SDK.

## Configuration format

Standard Claude Desktop layout, with optional sandrpod extensions:

```json
{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_..." }
    },
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/me/projects"],
      "sandrpod": {
        "alias": "fs",
        "restart_policy": "always",
        "max_restart_per_min": 5,
        "tool_denylist": ["delete_file"]
      }
    }
  }
}
```

### `sandrpod` sub-object fields

| Field | Default | Meaning |
|---|---|---|
| `enabled` | `true` | Set `false` to keep an entry in mcp.json but skip spawning. |
| `alias` | `<key>` | Namespace prefix; tools appear as `<alias>__<tool_name>`. |
| `restart_policy` | `always` | `always` \| `on-failure` \| `never`. |
| `max_restart_per_min` | `3` | Rate limit; child is marked `failed` after exhaustion. |
| `startup_timeout_sec` | `30` | Time allowed for spawn + initialize handshake. |
| `tool_allowlist` | (all) | If set, only these tools are exposed. |
| `tool_denylist` | (none) | These tools are removed even if allowlisted. |

Other tools (Claude Desktop, Cursor) ignore the `sandrpod` sub-object, so the
same file works everywhere.

## How tool names get prefixed

Each child's tools are exposed as `<alias>__<original_name>`. So GitHub's
`list_issues` becomes `github__list_issues`. The LLM sees the prefixed
name and you preserve the namespace; the bridge strips the prefix before
forwarding to the child.

Long aliases (>16 chars) are truncated with a deterministic hash suffix
so distinct long aliases never collide. Name collisions across servers
(rare) are broken by appending `__from_<server-name>` to the second
arrival, in alphabetical server order.

## Hot reload

Save a change to `mcp.json` and the bridge re-loads automatically:

- New entries are spawned.
- Removed entries are stopped.
- Changed entries (different command / args / env / sandrpod opts) are
  restarted.
- Unchanged entries keep their existing subprocess.

Connected MCP clients receive a `notifications/tools/list_changed` event and
should re-list tools to pick up the new set.

To disable: `sandrpod-agent --mcp-hot-reload=false`.

## Crash recovery

Every ready child is pinged every 20 seconds. If a child dies (process
crash, hang, OOM), the bridge:

1. Marks the child `failed` and removes its tools from the aggregate.
2. Notifies clients via `tools/list_changed`.
3. Restarts according to `restart_policy`, subject to `max_restart_per_min`.
4. Re-publishes the tools when the new child handshakes successfully.

A child that exhausts its restart budget stays `failed`. Check
`/mcp/manifest`'s `last_error` field to see why.

## Permission gate (employee-PC mode)

When `sandrpod-agent` runs with `--permission-mode=prompt` or `=strict`, the
bridge wires itself to the same consent flow as filesystem access:

| Event | Default behavior |
|---|---|
| `mcp.spawn` (first time a server starts) | Prompt: "Run MCP server X with command Y?" |
| `mcp.call` (every tool invocation) | Allowed if the user granted "permanent" earlier; otherwise prompts. |
| `mcp.restart` | No prompt — rate-limited by the policy above. |

Permanent grants are persisted in `~/.sandrpod/mcp_grants.json` (separate
file from `permissions.json` to keep schemas stable). Delete a server's
entry there to revoke.

`--permission-mode=off` skips all prompts (everything allowed). Use this
only when the network boundary is your security model (e.g. local LAN
deployments).

## Audit

Every `mcp.spawn`, `mcp.call`, `mcp.restart` event is written to the same
NDJSON audit log the path-permission gate uses (`~/.sandrpod/audit/` by
default). Fields:

- `source` — `mcp.spawn` / `mcp.call` / `mcp.restart`
- `path` — `mcp:<server>` (a grouping key, not a real path)
- `caller` — `mcp.bridge` (+ `:<tool>` for calls)
- `decision` — `allow` / `deny`
- `reason` — `status=ok duration_ms=412` for successful calls; failure
  details otherwise.

**Never logged**: tool arguments, return values, env-var values. The audit
records the *fact* of a tool call, not its content.

## Server-side (remote agent setup) {#SERVER}

When `sandrpod-agent` connects to a remote API Server, MCP traffic is
proxied through the existing yamux tunnel:

```
client →  POST /api/v1/sandboxes/{name}/mcp  →  API Server  →  tunnel  →  agent /mcp
```

The API Server uses a streaming-aware proxy (`proxyHTTPStreaming`) that
flushes after each chunk, so SSE upgrade works end-to-end.

Authentication: any caller that can hit the sandbox via the existing
sandbox-auth path can use the bridge. There is no separate MCP-level token.

## Tray integration

When `sandrpod-tray serve` is running, a new "MCP 服务" submenu appears
with per-server state and a "重载 mcp.json" entry. The tray talks to the
agent over `~/.sandrpod/mcp.sock` (Unix-socket HTTP), so it only works
when both are running on the same machine.

## Troubleshooting

**`bridge disabled` in agent logs** — `mcp.json` is missing at the
default location. Run `sandrpod-agent --mcp-config=/path/to/mcp.json`
explicitly or copy your Claude config (see Quick start).

**A server shows `state: failed`** — check `last_error` in
`/mcp/manifest`. Most common causes: missing required env var, command
not on PATH, npm registry unreachable for `npx -y` first-run, exceeded
`max_restart_per_min`.

**Claude Desktop and SandrPod fighting over the same MCP server** — they
both fork-exec the stdio child. Some servers hold exclusive resources
(SQLite file locks, Notion rate limits, …) and will fail one of the
clients. Either run only one at a time, or give SandrPod a separate
`mcp.json` with its own dedicated copies of the affected servers.

**Tools missing after hot reload** — connected clients are expected to
re-call `tools/list` on a `tools/list_changed` notification. If your
client doesn't, restart the session.

**Long tool names rejected by OpenAI / Anthropic API** — the bridge
truncates aliases over 16 chars to keep `alias__tool` under the 64-char
limit. If you're still hitting it, set a short `sandrpod.alias` in the
config.

## Migrating from `mcp-proxy` / `supergateway`

In `--mcp-only` mode the bridge is a drop-in replacement with bonus
features:

| | mcp-proxy | supergateway | SandrPod bridge |
|---|---|---|---|
| Multi-server aggregation | one endpoint per server | path-segmented | single endpoint with namespace prefix |
| Permission gate | — | — | optional, reuses existing tray |
| Audit | — | — | NDJSON + optional upload |
| Remote (cross-network) access | — | — | via API Server tunnel |

Pointing existing clients: the URL changes from
`http://localhost:8765/mcp` (mcp-proxy) to
`http://localhost:7090/mcp`. The protocol is identical.

## See also

- [`docs/MCP_TRANSPORT_BRIDGE_DESIGN.md`](MCP_TRANSPORT_BRIDGE_DESIGN.md) —
  full design rationale and architecture
- [`pkg/mcpbridge/`](../pkg/mcpbridge/) — Go API for embedding
- [`pkg/sdk/python/langchain_sandrpod/examples/04_personal_mcp.py`](../pkg/sdk/python/langchain_sandrpod/examples/04_personal_mcp.py)
  — LangChain integration sample
