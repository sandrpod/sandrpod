# SandrPod MCP Server

> **Status**: shipped (`/mcp` on the control plane, `/mcp` in every sandbox).
> Updated: 2026-11

SandrPod answers **Model Context Protocol** directly. An agent client — Claude
Desktop, Cursor, Cline, anything that speaks MCP — can create sandboxes, run
commands, read and write files, snapshot and fork them, and look at a desktop,
without an SDK.

There are two endpoints, and they are different products:

| Endpoint | What it is for | Auth |
|---|---|---|
| `https://<your-control-plane>/mcp` | The **platform**: create/list/stop/delete sandboxes, exec, files, preview URLs | API key (Bearer) |
| `https://<your-control-plane>/api/v1/sandboxes/<name>/toolbox/mcp` | **Inside one sandbox**: the desktop tools, plus whatever MCP servers the sandbox has configured | API key (Bearer) |

## Connecting a client

Claude Desktop (`claude_desktop_config.json`) and Cursor (`~/.cursor/mcp.json`)
take the same shape:

```json
{
  "mcpServers": {
    "sandrpod": {
      "url": "https://cloud.example.com/mcp",
      "headers": { "Authorization": "Bearer sk_..." }
    }
  }
}
```

The console's **接入** page generates this with your key pasted in, and lists the
tools the endpoint currently serves (read live, so it never goes stale).

## Platform tools

| Tool | Arguments | Notes |
|---|---|---|
| `sandbox_create` | `name`, `image?`, `cpu?`, `memory?`, `ttl?`, `node?`, `region?`, `gpu?`, `persist?` | Returns as soon as the record exists; poll `sandbox_list` for `RUNNING` |
| `sandbox_list` | — | State, node, image, resources |
| `sandbox_exec` | `sandbox`, `code`, `language?`, `timeout?` | Returns `stdout`, `stderr`, `exit_code` |
| `sandbox_read_file` | `sandbox`, `path` | Text |
| `sandbox_write_file` | `sandbox`, `path`, `content` | Creates parent dirs |
| `sandbox_list_files` | `sandbox`, `path?` | Defaults to `/workspace` |
| `sandbox_start` | `sandbox` | Resumes a stopped sandbox; data intact |
| `sandbox_stop` | `sandbox` | Releases CPU/memory (and GPUs), keeps `/workspace` |
| `sandbox_delete` | `sandbox` | Deletes the sandbox *and* its workspace |
| `sandbox_snapshot` | `sandbox`, `image?` | Commits the filesystem to an image |
| `sandbox_preview_url` | `sandbox`, `port` | A **signed** URL (2h) that opens the port |

A fork is `sandbox_snapshot` followed by `sandbox_create` with
`image=<snapshot>`: the image layers *and* the workspace volume come across.

## Sandbox tools (desktop)

`desktop_screenshot` (returns a PNG image block), `desktop_click`,
`desktop_type`, `desktop_key`, `desktop_scroll`. They are advertised by every
sandbox; a headless one answers
`this sandbox has no desktop — create it with a desktop image`, so an agent can
ask the question instead of guessing.

## Why every tool goes through REST

Each tool re-dispatches a copied request into the same mux that serves
`/api/v1`. Authorization, tenant scoping, validation and the persistence rules
are therefore the ones the API already enforces — the MCP layer cannot drift
from the REST surface or bypass it.

## Related

- [DESKTOP.md](DESKTOP.md) — desktop sandboxes, CDP, preview authentication
- [GPU.md](GPU.md) — GPU sandboxes
- [MCP_BRIDGE.md](MCP_BRIDGE.md) — putting *your own* stdio MCP servers inside a sandbox
