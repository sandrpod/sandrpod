# E2B MCP Gateway compatibility

> 中文版 / Chinese version: [E2B_MCP_COMPAT.zh.md](E2B_MCP_COMPAT.zh.md)
> — catalog table, config translation details, and the routing walkthrough.

An unmodified E2B SDK that does `Sandbox.create({ mcp })` and then
`getMcpUrl()` / `getMcpToken()` works against SandrPod. This page says how, and
states the one place the behaviour genuinely differs.

## Short version

Same mechanism, fully compatible client contract, **different tool launch
mechanism**.

- **Transport surface** (URL shape, token, Bearer auth, Streamable-HTTP) —
  drop-in.
- **Custom servers** (`{installCmd, runCmd}`) — fully supported.
- **Docker MCP Catalog entries** (`{exa: {apiKey}}`) — supported for a curated
  set, not all 200+. E2B runs each catalog tool as a **Docker container**;
  SandrPod runs it as an **stdio subprocess** (npx/uvx). Same tool, different
  way of starting it.

## This does not replace the native MCP bridge

Nothing about SandrPod's own MCP support changed. A sandbox has two independent
MCP surfaces:

| | Native SandrPod MCP | E2B-compatible MCP |
|---|---|---|
| Mounted at | toolbox `:8080/mcp` | mcp-gateway `:50005/mcp` |
| Reached via | `/api/v1/sandboxes/{name}/toolbox/mcp` | `50005-<id>.<domain>/mcp` |
| Config file | `/workspace/.sandrpod/mcp.json` | `/etc/mcp-gateway/config.json` |
| Auth | `SANDRPOD_MCP_TOKEN` (optional) | `GATEWAY_ACCESS_TOKEN` (Bearer, E2B's contract) |
| Runs | from container start | only when something runs `mcp-gateway` |

The config files are deliberately separate so neither clobbers the other.

`cmd/mcp-gateway` is not a second MCP implementation — it is a thin shim that
translates E2B's config shape and then hands off to the same `pkg/mcpbridge`
engine the native surface uses.

## How a request gets there

```
Sandbox.create({ mcp })
  → SDK runs `mcp-gateway --config …` inside the sandbox
  → shim listens on :50005, writes /etc/mcp-gateway/.token

getMcpUrl()   → https://50005-<id>.<domain>/mcp
              → server's generic port proxy → tunnel
              → toolbox /proxy/50005/ → 127.0.0.1:50005

getMcpToken() → files.read('/etc/mcp-gateway/.token')
```

The client then connects with `Authorization: Bearer <token>` exactly as it
would against E2B.

## Config shapes accepted

`translateConfig` in `cmd/mcp-gateway/config.go` takes three:

| Shape | Example | Handling |
|---|---|---|
| SandrPod native | `{"mcpServers":{"fs":{"command":"npx",…}}}` | used as-is |
| E2B custom server | `{"weather":{"installCmd":"pip install x","runCmd":"python -m s"}}` | `sh -c "install && run"` as an stdio child; an `owner/repo` key is `git clone`d first |
| E2B Docker Catalog | `{"exa":{"apiKey":"…"}}` | looked up in the curated list → npx/uvx child with credentials injected as env |

Curated catalog entries include `exa`, `brave`/`brave-search`,
`github`/`github-official`, `airtable`, `browserbase`, `slack`, and
`filesystem`, each mapped to a real npm package and its credential env vars.
Credential keys not in the mapping fall back to `UPPER_SNAKE_CASE`
(`geminiApiKey` → `GEMINI_API_KEY`). A catalog name that is not in the list is
skipped with a warning telling you to use the explicit `{installCmd, runCmd}`
form — catalog image names follow no guessable pattern (`brave` →
`mcp/brave-search`, `github-official` → `ghcr.io/…`), so guessing would be
worse than declining.

Full table: [E2B_MCP_COMPAT.zh.md](E2B_MCP_COMPAT.zh.md).

## The one real difference

E2B runs each catalog tool in its own Docker container. A SandrPod poder
sandbox is itself a container with no Docker inside it, so catalog tools run as
stdio subprocesses — the toolbox image ships node/npm and uv/uvx for exactly
this.

The tool behaves identically, because it is the same MCP server implementation.
What differs is coverage: curated entries work unchanged, anything else needs
an explicit `runCmd` (which always works and needs no Docker). Reproducing
one-container-per-tool would require Docker-in-Docker inside the sandbox; the
stdio path covers nearly everything and is far lighter, so that is not planned.

## Scope: container sandboxes only

This works for **poder (Docker container) sandboxes**, because the
`mcp-gateway` binary is only copied into the toolbox image.

It does **not** work automatically on **agent sandboxes** — a machine
registered directly by `sandrpod-agent`. The routing half is fine (the server
proxies `direct://` sandboxes to the agent's `/proxy/<port>/`), but
`sandrpod-agent` is a separate binary that does not bundle `mcp-gateway`, so
`create({mcp})` gets command-not-found and nothing listens on `:50005`.

For employee-PC mode that is the safer outcome: silently starting MCP tools on
someone's laptop is at odds with the whole point of the permission gate, and
command-not-found is fail-closed. If you do want it on a particular machine,
put `mcp-gateway` on that machine's PATH yourself — the permission gate still
applies.
