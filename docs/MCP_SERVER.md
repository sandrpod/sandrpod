# SandrPod MCP Server

> **Status**: shipped (`/mcp` on the control plane, `/mcp` in every sandbox).
> Updated: 2026-11

SandrPod answers **Model Context Protocol** directly. An agent client — Claude
Desktop, Cursor, Cline, anything that speaks MCP — can create sandboxes, run
commands, read and write files, snapshot and fork them, and look at a desktop,
without an SDK.

There are two endpoints, and they are different products — not two spellings of
one:

| Endpoint | Tools | Who it is for |
|---|---|---|
| `https://<your-control-plane>/mcp` | 15: lifecycle, exec, code interpreter, files, snapshot, preview, **and `sandbox_mcp_list` / `sandbox_mcp_call`** to discover and invoke any tool inside any sandbox | Default for every client (Claude Desktop, Cursor, Cline) — configure one static URL and reach everything |
| `https://<your-control-plane>/api/v1/sandboxes/<name>/mcp` | Direct access to *that* sandbox's MCP bridge (desktop tools, headless browser tools, `mcp.json` servers, resources / MCP Apps `ui://`) | A harness that wants first-class native schemas / MCP Apps for one sandbox, or single-sandbox least-privilege scoping |

Both authenticate the same way (the platform API key), and both are tenant-scoped:
a credential can only reach its own sandboxes, answered as `404` rather than `403`
so a stranger learns nothing about the name.

### Which one to configure

| Question | Platform entry | Sandbox entry |
|---|---|---|
| Can the client add an MCP server at runtime? | irrelevant — configure once | required, since the URL contains the sandbox name |
| Needs to create/list/stop sandboxes | ✅ | ❌ (no lifecycle tools) |
| Wants to drive desktop / browser / third-party MCP servers inside a sandbox | ✅ via `sandbox_mcp_list` + `sandbox_mcp_call` (credentials still stay in the sandbox) | ✅ native top-level tool schemas + MCP Apps (`ui://`) |
| Single-sandbox least-privilege isolation | ❌ (API key can see workspace's sandboxes) | ✅ scoped to one sandbox |

The default setup is the platform entry alone: one URL lets you create any sandbox
(code, desktop, headless browser, or one with your `mcp.json` servers) and call both
its sandbox primitives and its internal MCP tools over the same connection. Use the
sandbox entry directly when you want top-level native tool schemas, MCP Apps UI
resources, or single-sandbox scoping.

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
| `sandbox_run_code` | `sandbox`, `code`, `context?`, `timeout?` | A **stateful** kernel: variables survive between calls; matplotlib figures come back as image blocks |
| `sandbox_contexts` | `sandbox`, `action`, `context?` | `list` / `create` / `restart` / `remove` a kernel |
| `sandbox_mcp_list` | `sandbox`, `mcp_token?` | Discover MCP tools inside a sandbox (desktop, headless browser, and `mcp.json` servers) |
| `sandbox_mcp_call` | `sandbox`, `tool`, `arguments?`, `mcp_token?` | Call any MCP tool inside a sandbox; image blocks and errors pass straight through |

A fork is `sandbox_snapshot` followed by `sandbox_create` with
`image=<snapshot>`: the image layers *and* the workspace volume come across.

## Data analysis: the code interpreter

`sandbox_exec` runs a shell command in a fresh shell — nothing survives. For an
analysis loop that is the wrong tool: load a CSV once, inspect it, plot it, refine
the plot. `sandbox_run_code` runs in a **kernel** (the same one the E2B code
interpreter uses), so variables, imports and loaded data persist:

```text
sandbox_run_code{sandbox:"box", code:"df = pd.read_csv('/workspace/sales.csv')"}
sandbox_run_code{sandbox:"box", code:"df.describe()"}        # same df
sandbox_run_code{sandbox:"box", code:"df.plot(); plt"}       # returns a PNG
```

The result carries whatever the cell produced: stdout, the value of the last
expression, a traceback, and **every figure as an image block** — the client draws
the chart instead of printing base64. `sandbox_contexts` manages the namespaces
when one is not enough (a clean kernel per experiment, a restart without deleting
the sandbox).

The kernel is the same object the E2B gateway drives, so the two surfaces cannot
disagree about behaviour.

## How sandbox MCP calls are forwarded

`sandbox_mcp_list` and `sandbox_mcp_call` forward JSON-RPC calls to `/api/v1/sandboxes/{sandbox}/mcp` through the same mux (so tenant scoping and both container/agent routing apply unchanged) and pass the bridge's result straight through — an image block stays an image block. The platform caches the `Mcp-Session-Id` per `(caller, sandbox)` for 5 minutes (transparently re-initializing once if the sandbox restarted), and strips the platform API key before forwarding so credentials never leak into the sandbox.

## Why every tool goes through REST

Each tool re-dispatches a copied request into the same mux that serves
`/api/v1`. Authorization, tenant scoping, validation and the persistence rules
are therefore the ones the API already enforces — the MCP layer cannot drift
from the REST surface or bypass it.

## Related

- [DESKTOP.md](DESKTOP.md) — desktop sandboxes, CDP, preview authentication
- [GPU.md](GPU.md) — GPU sandboxes
- [MCP_BRIDGE.md](MCP_BRIDGE.md) — putting *your own* stdio MCP servers inside a sandbox
