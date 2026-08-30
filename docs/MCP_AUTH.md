# Protecting the MCP bridge

> 中文版 / Chinese version: [MCP_AUTH.zh.md](MCP_AUTH.zh.md)

Every SandrPod sandbox runs an **MCP bridge** in its toolbox, aggregating the
stdio and remote MCP servers listed in `mcp.json` into one Streamable-HTTP
`/mcp` endpoint. This page is about guarding that endpoint, and about the
reverse direction — the bridge authenticating to upstreams that require OAuth.

## Two layers, two headers

The endpoint is `<api-server>/api/v1/sandboxes/{name}/mcp`, reached through the
reverse tunnel. Two independently switchable layers guard it:

| Layer | Header | Checked by | Answers |
|---|---|---|---|
| **Platform** | `X-Sandrpod-Token: <platform token>` | the API Server | "may you reach this sandbox at all" |
| **Resource** (optional) | `Authorization: Bearer <mcp_token>` | the bridge inside the sandbox | "may you invoke the MCP tools on this machine" |

The API Server does not consume the second header — it passes it through the
tunnel untouched.

**Why two headers rather than one:** a single `Authorization` cannot carry two
secrets. `Authorization` is left to the MCP resource layer, because that is
where a generic MCP client naturally puts endpoint credentials, and the
platform token moves to `X-Sandrpod-Token`.

## The resource layer is optional — pick by tenancy

`mcp_token` is a shared secret for the bridge, rotatable independently of
platform auth. It defends one specific case: **when the platform operator is
not the owner of the MCP tools.** Being a platform admin should not by itself
let you trigger tools holding someone else's local credentials — their GitHub,
their Notion, their filesystem.

| Deployment | Recommendation |
|---|---|
| **Single tenant / self-hosted** — you run both the server and the sandboxes, one trust domain | **Do not set** `mcp_token`. The tunnel plus platform auth is already the boundary; a second layer is pure friction. |
| **Multi-tenant / employee PCs / BYO device** — operator ≠ tool owner | **Set** `mcp_token`, one per sandbox, rotated independently. A compromised API Server can then replay but not forge. |

```bash
sandrpod-agent -mcp-token <secret> …      # or SANDRPOD_MCP_TOKEN=<secret>
```

With no token the bridge logs a warning — *"any caller that reaches /mcp can
invoke tools"*. That is the deliberate single-tenant default (fail-open, with
the tunnel as the boundary), not an oversight.

## The manifest is exempt by default

`GET /mcp/manifest` returns read-only metadata — server names, states, tool
counts, **no credentials**. It is exempt from `mcp_token`: passing platform
auth is enough to see *which* tools exist, while *invoking* them still needs
the resource secret. This is least-privilege, and it keeps metadata queries
like `sandrpod-cli mcp tools` from needing a per-sandbox secret.

If even the tool list is sensitive:

```bash
sandrpod-agent -mcp-token <secret> -mcp-guard-manifest …
```

## Sending both

```bash
sandrpod-cli mcp tools <sandbox> --mcp-token <mcp_token>
```

```python
sb = SandrPodSandbox(sandbox_name="…", api_token="<platform>", mcp_token="<personal>")
```

```bash
curl <api-server>/api/v1/sandboxes/<name>/mcp/manifest \
  -H "X-Sandrpod-Token: <platform token>" \
  -H "Authorization: Bearer <mcp_token>"     # omittable for manifest; required for /mcp
```

## OAuth upstreams (Notion, Linear, GitHub official endpoints)

The reverse direction: the bridge as an MCP *client*, connecting to an upstream
that implements the MCP authorization spec — OAuth 2.1 with PKCE and dynamic
client registration. Endpoints like `mcp.notion.com` have no "paste an API key"
option. Supported natively, opt-in per entry:

```json
{ "mcpServers": { "notion": { "url": "https://mcp.notion.com/mcp", "auth": "oauth" } } }
```

One browser authorization, then unattended:

1. The child starts with no token, enters **`waiting_auth`** (the supervisor
   leaves it alone), and completes discovery plus dynamic registration.
2. The agent opens the authorization URL in the system browser. That URL is
   also exposed on the **local admin socket** — the public `/mcp/manifest`
   shows only the `waiting_auth` state and never the URL, because the browser
   handoff belongs to the local user session.
3. You approve; the callback lands on the bridge's **loopback listener**
   (default `127.0.0.1:7099/callback`), PKCE exchanges for a token, and it is
   stored at `~/.sandrpod/oauth/<server>.json` (0600, including the refresh
   token, never leaving the machine). The child restarts and goes `ready`.
4. Expiry is handled by automatic refresh from then on.

Optional `"oauth": {"client_id": …, "client_secret": "${ENV}", "scopes": […]}`
for services without dynamic registration.

**Scope:** this is an **agent-first** feature. The callback is loopback, so a
browser has to be able to reach it. The same machinery is wired into the
toolbox container, but a browser cannot reach a container's loopback, so the
flow cannot complete there — use static `headers` or an stdio `mcp-remote` shim
inside containers.

**Why it is not automatic for every `url` entry:** mcp-go's OAuth transport
does not send a request at all when it has no token, so enabling it everywhere
would push every ordinary unauthenticated remote into `waiting_auth`. Hence the
explicit `"auth": "oauth"`.

## See also

- [MCP_BRIDGE.md](MCP_BRIDGE.md) — the bridge itself: `mcp.json`, aggregation, hot reload
- [AUTH_AND_KEYS.md](AUTH_AND_KEYS.md) — issuing and managing platform tokens
- Code: `pkg/mcpbridge/auth.go`, `pkg/mcpbridge/oauth.go`, `cmd/agent`
