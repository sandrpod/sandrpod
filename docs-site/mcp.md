# MCP: bring your tools into the sandbox

> Every SandrPod sandbox ships with an **MCP bridge** that aggregates the MCP
> servers in your `mcp.json` (GitHub, Notion, Linear, filesystem…) into a
> single Streamable-HTTP endpoint. Any MCP client can use them — and your
> credentials **stay inside the sandbox**.

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
  --command npx -- -y @modelcontextprotocol/server-github \
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

External MCP clients get two entries:

### Tunnel endpoint (authenticated, recommended)

```
https://cloud.sandrpod.com/api/v1/sandboxes/{name}/mcp
```

```
X-Sandrpod-Token: <your API key>         # create one in the console
Authorization: Bearer <mcp_token>        # optional, see the security model
```

### Preview URL (no headers)

```
https://8080-{sandbox_name}.sandbox.sandrpod.com/mcp
```

The hostname is the capability (same model as E2B). Convenient for clients
that cannot attach custom headers; use the tunnel endpoint for sandboxes
running sensitive tools.

Client configuration (Cursor / Claude Desktop):

```json
{
  "mcpServers": {
    "my-tools": {
      "url": "https://8080-my-sandbox.sandbox.sandrpod.com/mcp",
      "transport": "streamable_http"
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
