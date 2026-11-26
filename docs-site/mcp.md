# MCP: bring your tools into the sandbox

> Every SandrPod sandbox ships with an **MCP bridge** that aggregates the MCP
> servers in your `mcp.json` (GitHub, Notion, Linear, filesystem…) into a
> single Streamable-HTTP endpoint. Any MCP client can use them — and your
> credentials **stay inside the sandbox**.

## Two endpoints, and why

SandrPod answers MCP in two places. They are not duplicates — pick by whether your
client can add a server at runtime.

| | **Platform entry** `/mcp` | **Sandbox entry** `/api/v1/sandboxes/<name>/mcp` |
|---|---|---|
| URL | one, fixed | contains the sandbox name |
| Tools | 15: create/list/stop/delete, exec, code interpreter, files, snapshot, preview, **`sandbox_mcp_list` / `sandbox_mcp_call`** | Every tool inside *that* sandbox (desktop, headless browser, `mcp.json` servers) + MCP Apps (`ui://`) |
| Create a sandbox | ✅ | — |
| Drive desktop / browser / `mcp.json` tools | ✅ via `sandbox_mcp_list` + `sandbox_mcp_call` | ✅ native top-level tool schemas |
| Your GitHub/Notion tokens | ✅ stay inside the sandbox (control plane only forwards JSON-RPC) | ✅ never leave the sandbox |
| Best for | Claude Desktop, Cursor, Cline (one static URL) | single-sandbox least-privilege scoping, native schemas, or MCP Apps UI |

The default setup is the platform entry alone: one URL lets you create a sandbox (code, desktop, headless browser, or with your `mcp.json` servers), discover its internal MCP tools with `sandbox_mcp_list`, and call them with `sandbox_mcp_call` over the same connection. Use the sandbox entry directly when you want native top-level tool schemas, MCP Apps (`ui://`), or single-sandbox isolation.

Both entries authenticate with the same platform API key and are tenant-scoped: a
credential reaches only its own sandboxes, and a stranger's name answers `404`, not
`403`.

## Data analysis, not just commands

`sandbox_exec` runs a command in a fresh shell — nothing survives. Data work is a
loop: load once, inspect, plot, refine the plot. `sandbox_run_code` runs in a
**kernel**, the same one the E2B code interpreter drives, so state persists:

```text
sandbox_run_code{sandbox:"box", code:"df = pd.read_csv('/workspace/sales.csv')"}
sandbox_run_code{sandbox:"box", code:"df.describe()"}      # same df, no reload
sandbox_run_code{sandbox:"box", code:"df.plot(); plt"}     # a PNG image block
```

A figure comes back as an **image block**, not as base64 text, so the client draws
it. `sandbox_contexts` gives each experiment its own namespace (`list` / `create` /
`restart` / `remove`).

## The problem it solves

Personal agents (Muse, Claude, your own agent) need to operate on *your*
knowledge base, *your* code, *your* files — but handing credentials to a
hosted platform costs trust.

SandrPod keeps credentials inside your own sandbox. The platform forwards
tool calls in; it never sees the token.

| | Hosted MCP | SandrPod sandbox MCP |
|---|---|---|
| Where credentials live | Platform database | **Inside the sandbox** (0600) |
| Can the platform read tokens | Yes | **No** (sees a one-time code only) |
| Config format | Varies | Claude Desktop format, as-is |
| Tool surface | One service | N services, one endpoint |

## Quick start (3 minutes)

### 1. Add an MCP server

```bash
# Standard OAuth services (Notion / Linear / GitHub / Sentry)
sandrpod-cli mcp add my-sandbox notion \
  --url https://mcp.notion.com/mcp

# Local stdio servers (Claude Desktop config works as-is)
sandrpod-cli mcp add my-sandbox github \
  "npx -y @modelcontextprotocol/server-github" \
  --env GITHUB_PERSONAL_ACCESS_TOKEN=ghp_…
```

### 2. Authorize (OAuth services only)

```bash
sandrpod-cli mcp ls my-sandbox
#   notion  →  https://mcp.notion.com/mcp  [waiting_auth]
#     ↳ Authorize: https://mcp.notion.com/authorize?client_id=…
```

Open the link and click Allow. The callback routes **directly into the
sandbox** (via the preview URL), the bridge completes the token exchange,
and the state flips to `ready`.

From the console: sandbox detail → **MCP** tab → Authorize.

### 3. Use it

```bash
sandrpod-cli mcp tools my-sandbox        # list all tools
sandrpod-cli mcp url my-sandbox          # endpoint + auth headers
```

## External endpoints

External MCP clients reach the native bridge through the authenticated tunnel:

### Tunnel endpoint (recommended)

```
https://cloud.sandrpod.com/api/v1/sandboxes/{routing-id}/mcp
```

```
X-Sandrpod-Token: <your API key>         # create one in the console
Authorization: Bearer <mcp_token>        # optional, see the security model
```

There is deliberately **no unauthenticated URL** for this bridge: it lives on the
toolbox's `:8080`, and the public port proxies never expose `:8080` because that
control plane has no per-sandbox credential. The E2B gateway's own
`mcp-gateway` is a different endpoint — it listens on `:50005` and is reached at
`https://50005-<routing-id>.<domain>/mcp` (see
[E2B MCP compatibility](https://github.com/sandrpod/sandrpod/blob/main/docs/E2B_MCP_COMPAT.md)).

Client configuration (Cursor / Claude Desktop):

```json
{
  "mcpServers": {
    "my-tools": {
      "url": "https://cloud.sandrpod.com/api/v1/sandboxes/{routing-id}/mcp",
      "transport": "streamable_http",
      "headers": {
        "X-Sandrpod-Token": "<your API key>"
      }
    }
  }
}
```

## Configuration

`mcp.json` is byte-compatible with Claude Desktop / Cursor / Cline. SandrPod
extensions live under a `sandrpod` sub-object:

| Field | Default | Meaning |
|---|---|---|
| `alias` | key | Tool prefix: `<alias>__<tool_name>` |
| `restart_policy` | `always` | `always` / `on-failure` / `never` |
| `max_restart_per_min` | 3 | Child marked `failed` past the limit |
| `startup_timeout_sec` | 30 | Use 180 for first-time OAuth consent |
| `tool_allowlist` / `tool_denylist` | all / none | Tool filtering |
| `resource_*` / `prompt_*` | same | Independent filters for resources and prompts |

Three server types:

| Type | Config | Notes |
|---|---|---|
| stdio | `command` + `args` + `env` | Local subprocess (npx/uvx) |
| Streamable-HTTP | `url` | Remote MCP (Notion etc.) |
| OAuth remote | `url` + `auth: "oauth"` | Browser consent handled automatically |

## How OAuth works (credentials never leave the sandbox)

```text
1. The bridge detects an OAuth-only server and builds an authorization URL
   (PKCE + dynamic client registration) with
   redirect_uri = https://7099-{sandbox}.{domain}/callback
2. You click Allow in the browser
3. The provider redirects to the preview URL → Caddy → control plane →
   tunnel → sandbox
4. The bridge exchanges the one-time code **directly with the provider**
   (outbound HTTPS from the sandbox)
5. The token is stored at /workspace/.sandrpod/oauth/ (0600) and refreshed
   automatically
```

**The control plane only ever sees the one-time code.** Revoking = delete
the sandbox, or revoke the grant at the provider.

## Security model

| Layer | What it guards | Setup |
|---|---|---|
| Platform `X-Sandrpod-Token` | Can you **reach** the sandbox | API key from the console |
| Resource `Authorization: Bearer` | Can you **invoke** tools | worker `-mcp-token` (optional) |
| Permission gate (agent mode) | Per-call consent | `-permission-mode=prompt` |
| Audit | Who called what, when | automatic, exportable |

Multi-tenant / employee-PC deployments should **always** set `-mcp-token`:
a platform admin should not be able to fire tools that hold someone else's
credentials.

## E2B compatibility

`Sandbox.create({ mcp })` + `getMcpUrl()` / `getMcpToken()` work unchanged.
See [E2B drop-in](/e2b).

## FAQ

**Still waiting_auth after authorizing?**
Refresh or wait 30 seconds (the console polls). `mcp ls` shows the error if
it is stuck.

**Changed mcp.json but nothing happened?**
The bridge watches the file every 5 seconds. Check the path: the first line
of `sandrpod-cli mcp ls`.

**The OAuth page will not open?**
Ensure the worker runs with `SANDRPOD_E2B_DOMAIN` (the preview URL needs it).

**Where are the credentials; how do I delete them?**
`/workspace/.sandrpod/oauth/` (0600). Deleting the sandbox removes them; or
revoke the grant at the provider.

## Tool reference

Everything the platform entry serves, defaults included. Every `sandbox_*` tool takes
the sandbox **name**; `desktop_*` and `sandbox_run_code` also accept the kernel or
screen they act on.

| Tool | Arguments | What it does |
|---|---|---|
| `sandbox_create` | `name`, `image`, `cpu`, `memory`, `ttl`, `node`, `region`, `gpu`, `persist` | Creates the record; poll `sandbox_list` for RUNNING. `image` is how you get a desktop (`sandrpod-desktop:v6`) or a GPU-ready base (`sandrpod-toolbox:ubuntu`) |
| `sandbox_list` | — | State, node, image, resources |
| `sandbox_exec` | `sandbox`, `code`, `language`, `timeout` | One-shot shell; nothing survives |
| `sandbox_run_code` | `sandbox`, `code`, `context`, `timeout` | **Stateful** Python kernel; figures come back as images |
| `sandbox_contexts` | `sandbox`, `action`, `context` | `list` / `create` / `restart` / `remove` a kernel |
| `sandbox_read_file` | `sandbox`, `path` | Text |
| `sandbox_write_file` | `sandbox`, `path`, `content` | Creates parent directories |
| `sandbox_list_files` | `sandbox`, `path` | Defaults to `/workspace` |
| `sandbox_start` | `sandbox` | Resumes a stopped sandbox, data intact |
| `sandbox_stop` | `sandbox` | Releases CPU/memory/GPU, keeps `/workspace` |
| `sandbox_delete` | `sandbox` | Deletes the sandbox **and** its workspace |
| `sandbox_snapshot` | `sandbox`, `image` | Commits the filesystem to an image (and pushes it when a registry is configured) |
| `sandbox_preview_url` | `sandbox`, `port` | Signed URL (2 h) for a port inside the sandbox |
| `sandbox_mcp_list` | `sandbox`, `mcp_token?` | List MCP tools inside a sandbox (desktop, headless browser, `mcp.json` servers) |
| `sandbox_mcp_call` | `sandbox`, `tool`, `arguments?`, `mcp_token?` | Invoke any MCP tool inside a sandbox (image blocks pass straight through) |

The **sandbox entry** (`/api/v1/sandboxes/<name>/mcp`) exposes that sandbox's built-in tools (such as `desktop_*` on desktop images) plus every server in its `mcp.json` directly as top-level tools — those are the same tools `sandbox_mcp_list` / `sandbox_mcp_call` discover and invoke from the platform entry.

Nothing is inferred at runtime: a tool that is not in this table does not exist, and
`tools/list` on either entry is always the authority.
