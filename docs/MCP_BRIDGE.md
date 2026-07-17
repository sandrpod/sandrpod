# SandrPod MCP Transport Bridge — User Guide

> **Status**: shipped. Works standalone on a LAN (`-mcp-only`), on a directly
> registered agent, and remote-via-tunnel through the API Server
> (`/api/v1/sandboxes/{name}/mcp`, see §SERVER).

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

### Top-level entry fields (HTTP transport)

Besides stdio (`command`/`args`/`env`), an entry can point at a **remote HTTP
MCP server** directly — the field shape mirrors Claude Code's remote-server
config:

| Field | Meaning |
|---|---|
| `url` | Remote MCP endpoint; setting it makes the entry HTTP instead of stdio. |
| `type` | `streamable-http` (default) \| `http` \| `sse`. |
| `headers` | Static request headers; values support `${ENV}` expansion. |
| `auth` | `""` (default: static headers only) \| `"oauth"` — opt into the browser OAuth flow. |
| `oauth` | Optional `{client_id, client_secret, scopes}` for servers without dynamic client registration. |

## Connecting OAuth-only services (Notion, Linear, Sentry, …)

A growing class of services expose only a **remote, OAuth-authenticated**
MCP server and deliberately do **not** offer a static API token. Notion's
hosted server (`https://mcp.notion.com/mcp`) is the canonical example: it
requires per-user OAuth 2.1 (PKCE) and explicitly rejects bearer tokens.

**Native OAuth (recommended on agent hosts):** give the entry a `url` and
`"auth": "oauth"`. The bridge runs the browser-consent flow once (the child
parks in `waiting_auth` with an authorization URL), persists the token under
`~/.sandrpod/oauth/` (`0600`), and refreshes it unattended. Controlled by the
agent flags `-mcp-oauth` / `-mcp-oauth-callback` / `-mcp-oauth-token-dir`.
Full details in [MCP_AUTH.md](MCP_AUTH.md).

**Inside containers** (toolbox sandboxes) the loopback OAuth callback is not
reachable, so the browser flow can't complete there. Use the community
[`mcp-remote`](https://github.com/geelen/mcp-remote) stdio bridge instead,
which Notion's own docs recommend:

```jsonc
{
  "mcpServers": {
    "notion": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "https://mcp.notion.com/mcp"],
      "sandrpod": {
        // OAuth means: spawn → mcp-remote opens a browser → you click
        // "authorize" → token cached. That round-trip easily exceeds the
        // 30s default handshake window, so widen it.
        "startup_timeout_sec": 180
      }
    }
  }
}
```

What happens on first start:

1. The bridge forks `mcp-remote` like any other stdio child.
2. `mcp-remote` connects to the remote server, sees it needs OAuth, and
   **opens a browser on the local machine** (PKCE flow).
3. The employee — who is sitting at this machine — completes the consent.
4. `mcp-remote` caches the access + refresh tokens in **its own**
   `~/.mcp-auth/` directory and handles refresh/rotation itself.

Throughout, **the bridge handles zero OAuth tokens**. The OAuth flow and
all credential storage live entirely inside the `mcp-remote` subprocess;
sandrpod's role stays "fork stdio + relay JSON-RPC". This keeps the
privacy model clean — the credential chain simply doesn't include us.

> **Verified end-to-end**: Notion via `mcp-remote` comes up `ready` with
> its full tool set aggregated alongside other servers under the single
> `/mcp` endpoint (tools appear as `notion__*`).

**Caveat — interactive first run.** The browser consent in step 2-3
requires a human at the machine. This works precisely because, in the
employee-PC deployment, the person driving the remote AI *is* the person
at the keyboard. In an unattended/headless deployment (nobody at the
machine) the OAuth flow cannot complete — use a static-token server there
instead (e.g. for Notion, the older `@notionhq/notion-mcp-server` with
`NOTION_TOKEN`).

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
3. Waits an exponentially-growing backoff (1s → 2s → 4s → 8s → … capped
   at 30s) before respawning. This prevents a sick child (e.g. waiting
   on a slow upstream API) from burning its entire per-minute restart
   budget in one second of thrashing.
4. Restarts according to `restart_policy`, subject to `max_restart_per_min`.
5. Re-publishes the tools when the new child handshakes successfully.

A child that exhausts its restart budget stays `failed`. Check
`/mcp/manifest`'s `last_error` field to see why.

## Graceful shutdown

On SIGTERM/SIGINT the agent:

1. Stops accepting new HTTP connections (server close).
2. Stops the supervisor and fsnotify watcher so no late restart or
   reload kicks in mid-drain.
3. Waits up to **10 seconds** for in-flight `tools/call` invocations to
   complete (per-child `WaitDrain` on the in-flight WaitGroup).
4. Sends SIGTERM to remaining children and exits.

Calls that haven't returned by the drain deadline are abandoned and the
client sees a connection close. The drain bound prevents a hung MCP
server (e.g. a third-party API stuck behind a load-balancer) from
holding shutdown forever.

## Performance

Measured on Apple M1 Pro through the full HTTP path (10 fake servers ×
5 tools each):

| Scenario | Throughput | Latency p99 |
|---|---:|---:|
| Full HTTP → bridge → child round-trip | 4,400 req/s | < 10 ms |
| Dispatch only (no HTTP framing) | 6.4 M op/s | — |
| tools/list aggregation | 65 K op/s | — |

The design target was 10 servers × 100 req/s aggregate; the
implementation has ~40× headroom against that. Real-world throughput is
dominated by the upstream MCP server, not the bridge.

`TestLoad_10ServersX100PerSec` runs as part of `go test ./...` and
fails CI if throughput drops below 100 req/s or p99 exceeds 500 ms —
the bounds are loose so flaky CI machines don't trip them, but
catastrophic regressions (lock contention, leaks) will.

## Authentication

The bridge runs through two independent auth layers — one at the API
Server, one at the agent — and each uses a **different HTTP header** so
clients can pass both at once:

| Layer | Token | Header | Set on |
|---|---|---|---|
| API Server (`/api/v1/*` ingress) | `cfg.Token` | `X-Sandrpod-Token: <token>` | `sandrpod-server -token <…>` |
| Agent (`/mcp` endpoint after tunnel) | `--mcp-token` | `Authorization: Bearer <token>` | `sandrpod-agent --mcp-token <…>` |

The API Server validates `X-Sandrpod-Token` and forwards `Authorization`
**unchanged** through the tunnel. So even a compromised API Server cannot
forge new MCP calls — it never sees the MCP Bearer in plaintext, only
relays it. (It can still replay calls it intercepted on the wire; that's
the known residual risk of the proxy model.)

### Example client config

```python
from langchain_mcp_adapters.client import MultiServerMCPClient

client = MultiServerMCPClient({
    "personal": {
        "url": "https://your-server/api/v1/sandboxes/laptop-1/mcp",
        "transport": "streamable_http",
        "headers": {
            "X-Sandrpod-Token":  "api-token-abc",    # API Server auth
            "Authorization":     "Bearer mcp-xyz",    # forwarded to agent
        },
    },
})
```

### Three deployment modes

| Mode | When safe | Setup |
|---|---|---|
| **Both tokens** (recommended for production) | Multi-tenant or shared API Server | Server: `-token …`; agent: `--mcp-token …`; client sends both headers |
| **API token only** | Single-tenant API Server, no shared infra | Server: `-token …`; agent omits `--mcp-token`; client sends only `X-Sandrpod-Token` |
| **No auth** | Loopback `--mcp-only`, dev box | Omit both flags |

`GET /mcp/manifest` is exempt from the MCP token by default (it is read-only
metadata — server names and tool counts, no credentials — and stays reachable
with platform auth alone). Pass `-mcp-guard-manifest` to require the MCP token
there too.

### Legacy `Authorization: Bearer <cfg.Token>` clients

Pre-MCP-bridge clients (sandbox CRUD, exec, etc.) that put the API
token in `Authorization: Bearer` continue to work — the middleware
falls back to that scheme. But **for any call that goes to `/mcp`** the
client must switch to `X-Sandrpod-Token`, because the agent expects
`Authorization` to carry its own Bearer.

### Admin endpoints

The admin socket on `~/.sandrpod/mcp.sock` is **never** covered by
either of these tokens — Unix-socket file permissions (0600, same uid)
are the auth boundary for management operations.

## Permission gate (employee-PC mode)

When `sandrpod-agent` runs with `--permission-mode=prompt` or `=strict`, the
bridge wires itself to the same consent flow as filesystem access:

| Event | Default behavior |
|---|---|
| `mcp.spawn` (first time a server starts) | Prompt: "Run MCP server X with command Y?" |
| `mcp.call` on a **non-sensitive** tool | Allowed if the user granted "permanent" earlier; otherwise prompts. |
| `mcp.call` on a **sensitive** tool | Prompts every time, even after "allow permanent". Never persisted. |
| `mcp.restart` | No prompt — rate-limited by the policy above. |

**Sensitive tool patterns** (case-insensitive substring match on the
un-prefixed tool name): `delete`, `remove`, `drop`, `truncate`, `purge`,
`destroy`, `wipe`, `send`, `publish`, `post`, `transfer`, `pay`,
`charge`, `merge`, `revoke`, `reset`, `unsubscribe`.

Extend or override the list via env:

```bash
# Add to defaults (comma-separated):
SANDRPOD_MCP_SENSITIVE_PATTERNS=fire_,grant_admin

# Replace the defaults entirely (e.g. you don't care about merge_pr):
SANDRPOD_MCP_SENSITIVE_PATTERNS_OVERRIDE=delete,destroy,wipe,send_money
```

**Grant scope** (`-mcp-grant-scope`, env `SANDRPOD_MCP_GRANT_SCOPE`) sets
what a dialog "allow" covers for tool calls:

- **`server`** (default) — one allow covers every non-sensitive tool on
  that server; first use of a server prompts once, then it goes quiet.
  Persisted as the `"<server>:*"` wildcard.
- **`tool`** — every tool prompts once (grants recorded per `server:tool`).
  For deployments where each tool is a separately-audited capability.

Scope shapes what a click *writes*, never what a lookup honors: per-tool
entries, wildcards and session grants all keep working in either mode.

**Grant memory** (non-sensitive tools only — sensitive ones always prompt,
no cache applies, in both scopes):

- **Allow once** — this call only; next call prompts again.
- **Allow for session** — cached in memory (per scope key above); silent
  until the agent restarts. Never written to disk.
- **Allow permanently** — persisted in `~/.sandrpod/mcp_grants.json`
  (separate file from `permissions.json` to keep schemas stable). Delete
  an entry there to revoke.
- **Whole-server wildcard** — `"<server>:*": true` under `"tools"`. This
  is what server-scope allows write; in tool scope it stays available as
  an operator-authored edit. Sensitive tools still prompt through it.
- **Hand-edits apply live** — the agent re-checks the file (mtime/size)
  on every gated call, so edits take effect on the next call in BOTH
  directions: adding a grant silences it, deleting the file revokes every
  persistent grant. No agent restart needed. A file that fails to parse
  keeps the last good grants (never widens access) and logs once.

```json
{
  "version": 1,
  "servers": { "browser": true },
  "tools": { "browser:*": true, "gh:list_issues": true }
}
```

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

Authentication: see the Authentication section above. Short version:
API token in `X-Sandrpod-Token`, MCP Bearer in `Authorization` — the
two are independent and only the agent sees the MCP Bearer in plaintext.

## Tray integration

When `sandrpod-tray serve` is running, a new "MCP 服务" submenu appears
with per-server state and a "重载 mcp.json" entry. The tray talks to the
agent over `~/.sandrpod/mcp.sock` (AF_UNIX HTTP). Both processes must be
running as the same OS user on the same machine.

**Platform support**: AF_UNIX sockets work on macOS, Linux, and Windows
build 17134+ (Windows 10 1803 or later). On older Windows the agent logs
"admin socket disabled" and continues serving `/mcp` normally — only the
tray's MCP submenu shows "未连接".

## Troubleshooting

**`no servers loaded yet` in agent logs** — `mcp.json` doesn't exist
at the configured path. The bridge is still UP and watching for the
file: drop it in (`cp ~/Library/Application\ Support/Claude/claude_desktop_config.json
~/.sandrpod/mcp.json` on macOS) and the agent picks it up within ~250 ms
without a restart. `rm` of the file works the same way in reverse —
all children stop, manifest reports zero servers.

**A server shows `state: failed`** — check `last_error` in
`/mcp/manifest`. Most common causes: missing required env var, command
not on PATH, npm registry unreachable for `npx -y` first-run, exceeded
`max_restart_per_min`.

**An OAuth (`mcp-remote`) server fails or hangs on first start** — the
browser consent didn't complete in time. Bump `startup_timeout_sec` (180
is a good value), and make sure a browser actually opened on the machine.
If `~/.mcp-auth/` already holds a stale/expired grant, clear it and let
the flow run again. Remember the consent must be completed by a human at
*this* machine — it won't work in a headless deployment.

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

- [`docs/design/mcp-transport-bridge.md`](design/mcp-transport-bridge.md) —
  full design rationale and architecture
- [`pkg/mcpbridge/`](../pkg/mcpbridge/) — Go API for embedding
- [`pkg/sdk/python/langchain_sandrpod/examples/04_personal_mcp.py`](../pkg/sdk/python/langchain_sandrpod/examples/04_personal_mcp.py)
  — LangChain integration sample
