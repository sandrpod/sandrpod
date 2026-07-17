---
name: register-mcp
description: Register (add/remove/enable/disable) an MCP server in a SandrPod sandbox at runtime by editing mcp.json. The toolbox MCP bridge hot-reloads the file and exposes the new tools at /mcp — no restart. Use when you need a new MCP tool/server available inside the sandbox, or to list/remove already-registered ones.
---

# Register MCP tools in a SandrPod sandbox (via mcp.json)

The SandrPod toolbox runs an **MCP bridge** that aggregates stdio MCP servers
declared in `mcp.json` and serves their tools at the `/mcp` endpoint. The bridge
**watches `mcp.json` and hot-reloads on change**, so registering a new MCP tool
is just: *edit the JSON file → wait for reload → verify*. No process restart.

This is the runtime "interactive registration" mechanism: you (the AI) edit the
file; the bridge does the rest.

## 1. Find the config file

Resolution order (first match wins):

1. `$SANDRPOD_MCP_CONFIG` (or the `-mcp-config` flag the toolbox was started with)
2. `$XDG_CONFIG_HOME/sandrpod/mcp.json`
3. `~/.sandrpod/mcp.json`  ← default

```bash
CFG="${SANDRPOD_MCP_CONFIG:-${XDG_CONFIG_HOME:+$XDG_CONFIG_HOME/sandrpod/mcp.json}}"
CFG="${CFG:-$HOME/.sandrpod/mcp.json}"
echo "$CFG"
```

If the file does not exist yet, create its parent dir and start from `{"mcpServers":{}}`.
The bridge picks up a *newly created* file too.

## Available launch commands (what `command` can be)

The bridge spawns `command` + `args` as a plain stdio subprocess, so any
installed executable works. The SandrPod toolbox image ships these runtimes:

| Ecosystem | `command` | Example `args` | Notes |
|-----------|-----------|----------------|-------|
| Node | `npx` | `["-y", "@upstash/context7-mcp"]` | most common |
| Node | `node` | `["/workspace/my-server.js"]` | local script |
| Python | `uvx` | `["mcp-server-time"]` | de-facto launcher for Python MCP servers; auto-manages the env |
| Python | `python3` | `["-m", "my_module"]` | only if the module is already installed |
| Remote HTTP/SSE | *(use `url`)* | — | connect directly via `url`; see §2 (only OAuth servers need the `mcp-remote` command) |

Compiled (Go/Rust) MCP servers work too — point `command` at the binary path —
but the binary must already exist in the sandbox. `docker` is not available
(and running Docker inside the sandbox is not supported).

> First run of `npx -y` / `uvx` downloads the package, so it needs network
> access and a writable cache. Expect the server to take a few seconds to reach
> `state: "ready"` the first time.

## 2. Schema (complete)

This is the **full** `mcp.json` schema the SandrPod bridge parses. Each server
is either **stdio** (`command`) or **HTTP** (`url`) — exactly one. Unknown
fields are ignored.

```jsonc
{
  "mcpServers": {                       // the only top-level key
    "<server-name>": {
      // --- stdio transport (one of stdio/HTTP) ---
      "command": "npx",                 // executable to spawn
      "args": ["-y", "some-mcp-pkg"],   // optional: argv
      "env": { "API_KEY": "..." },      // optional: extra env vars for the child
      // --- OR HTTP transport ---
      "url": "https://host/mcp",        // remote endpoint (omit command when set)
      "type": "streamable-http",        // "streamable-http"(default) | "http" | "sse"
      "headers": { "Authorization": "Bearer ${TOKEN}" }, // optional; ${ENV} expanded
      "auth": "oauth",                  // optional: browser OAuth flow (agent hosts only, see below)
      "sandrpod": {                     // optional: bridge-specific knobs (all optional)
        "enabled": true,                // default true. false = kept in file but not spawned
        "alias": "myalias",             // default = <server-name>. tool namespace prefix
        "tool_allowlist": ["tool_a"],   // default none. if set, ONLY these tool names are exposed
        "tool_denylist": ["danger"],    // default none. these tool names are hidden
        "restart_policy": "always",     // "always" (default) | "on-failure" | "never"
        "max_restart_per_min": 3,       // default 3. restart-loop cap (sliding 1-min window)
        "startup_timeout_sec": 30       // default 30. handshake deadline before marking failed
      }
    }
  }
}
```

### Field reference

| Field | Required | Default | Meaning |
|-------|----------|---------|---------|
| `mcpServers` | yes | — | map of server-name → entry |
| `command` | one of command/url | — | stdio executable (npx / uvx / node / python3 / binary) |
| `args` | no | `[]` | arguments (stdio) |
| `env` | no | `{}` | environment variables for the child process (stdio) |
| `url` | one of command/url | — | remote MCP endpoint (HTTP) |
| `type` | no | `streamable-http` | HTTP transport: `streamable-http` / `http` / `sse` |
| `headers` | no | `{}` | HTTP request headers; values support `${ENV}` expansion |
| `auth` | no | `""` | `"oauth"` opts an HTTP entry into the browser OAuth flow — works on `sandrpod-agent` hosts; **not** inside containers (see the OAuth section) |
| `sandrpod.enabled` | no | `true` | `false` registers without spawning |
| `sandrpod.alias` | no | server-name | namespace prefix; tools appear as `<alias>__<tool>` |
| `sandrpod.tool_allowlist` | no | (none) | expose only these tool names |
| `sandrpod.tool_denylist` | no | (none) | hide these tool names |
| `sandrpod.restart_policy` | no | `always` | `always` / `on-failure` / `never` |
| `sandrpod.max_restart_per_min` | no | `3` | restart-loop guard |
| `sandrpod.startup_timeout_sec` | no | `30` | raise it for slow first-run downloads (npx/uvx) |

Tools are exposed namespaced as **`<alias>__<tool_name>`** (e.g. `notion__search`).

### Remote / HTTP MCP servers (native — no shim)

Set `url` (and optionally `type` / `headers`) to connect directly over
Streamable-HTTP (default) or SSE — no subprocess, no `mcp-remote`:

```jsonc
// no-auth or query-string-auth
"docs": { "url": "https://mcp.context7.com/mcp" },

// bearer / custom headers — values support ${ENV} expansion
"internal": {
  "url": "https://mcp.internal.example.com/mcp",
  "type": "streamable-http",
  "headers": { "Authorization": "Bearer ${INTERNAL_MCP_TOKEN}" }
},

// SSE transport
"legacy": { "url": "https://old.example.com/sse", "type": "sse" }
```

Header (and URL) `${VAR}` / `$VAR` placeholders are expanded from the server's
`env` map first, then the process environment — so secrets stay out of the file.

### OAuth-protected HTTP servers

Two recipes, depending on where the bridge runs:

- **On a `sandrpod-agent` host** (a real machine with a browser): use native
  OAuth — give the entry a `url` plus `"auth": "oauth"`. The bridge runs the
  browser-consent flow once (the entry parks in `waiting_auth` with an
  authorization URL), persists the token under `~/.sandrpod/oauth/`, and
  refreshes it unattended. See `docs/MCP_AUTH.md`.
- **Inside a toolbox container**: the loopback OAuth callback is unreachable
  there, so `auth:"oauth"` entries park in `waiting_auth` forever. Bridge with
  the stdio [`mcp-remote`](https://github.com/geelen/mcp-remote) shim instead,
  which handles the login and token cache:

```jsonc
"notion": {
  "command": "npx",
  "args": ["-y", "mcp-remote", "https://mcp.notion.com/mcp"]
}
```

### Claude config compatibility

The schema is aligned with Claude's MCP config on both shapes:

- **stdio** (`command`/`args`/`env`) — matches Claude Desktop; entries are
  drop-in copyable both ways (the extra `sandrpod` sub-object is ignored by
  Claude, and vice-versa).
- **HTTP** (`url`/`type`/`headers`) — matches Claude Code's remote-server shape.
  Caveat: `type` accepts `streamable-http` (default), `http`, or `sse`; OAuth is
  opt-in via `"auth": "oauth"` (agent hosts) or the `mcp-remote` recipe above
  (containers), not auto-negotiated.
- Top-level keys other than `mcpServers` (e.g. `imports`) are ignored.

## 3. Register a server (safe read-modify-write)

**Never clobber the file** — other servers may already be registered. Merge into
`mcpServers`, write atomically, validate JSON first.

```bash
CFG="$HOME/.sandrpod/mcp.json"
mkdir -p "$(dirname "$CFG")"
[ -f "$CFG" ] || echo '{"mcpServers":{}}' > "$CFG"

# Merge a new server with jq (atomic via temp file + mv)
jq '.mcpServers["context7"] = {
      "command":"npx",
      "args":["-y","@upstash/context7-mcp"]
    }' "$CFG" > "$CFG.tmp" && mv "$CFG.tmp" "$CFG"
```

Prefer `jq` for correctness. If `jq` is unavailable, read the whole file, parse,
mutate the `mcpServers` object, and write the full document back — do not append
fragments.

## 4. Verify it loaded

The bridge reloads within ~1–2s of the file change. Confirm via the manifest
(`<alias>__<tool>` entries appear when `state:"ready"`):

```bash
# Standalone toolbox:           http://<sandbox-host>:8080/mcp/manifest
# Via API server tunnel:        /api/v1/sandboxes/<name>/mcp/manifest
# (add Authorization: Bearer <mcp-token> if the bridge was started with one)
curl -s http://localhost:8080/mcp/manifest | jq '.servers[] | {name, state, tool_count}'
```

Poll until the new server shows `state: "ready"`. If it shows `error`/`backoff`,
read the `last_error` field for the reason (bad command, missing package, missing
env var, OAuth pending). First-run `npx -y` / `uvx` downloads can exceed the
30s default — bump `sandrpod.startup_timeout_sec` for slow servers.

> **Consumers must re-list tools.** The bridge does not push a
> `notifications/tools/list_changed` after a reload, so an MCP client already
> connected to `/mcp` won't see the new tools until it calls `tools/list` again
> (or reconnects). Newly-connecting clients see them immediately.

## 5. Other operations

- **Disable without removing**: set `.mcpServers["x"].sandrpod.enabled = false`, save. The bridge stops that child, keeps the entry.
- **Remove**: `jq 'del(.mcpServers["x"])' "$CFG" > "$CFG.tmp" && mv "$CFG.tmp" "$CFG"`.
- **List registered**: `jq '.mcpServers | keys' "$CFG"`, or query `/mcp/manifest` for live state.
- **Avoid name clashes**: set a unique `sandrpod.alias` so tools don't collide with existing `<alias>__*` names.

## Guardrails

- Validate JSON before saving; a malformed `mcp.json` makes the bridge keep the
  last good config and log a parse error.
- Only spawn MCP servers you trust — a registered stdio server runs as a real
  subprocess inside the sandbox with the sandbox's privileges. The container is
  the security boundary.
- Don't store long-lived secrets in `mcp.json` in plaintext when an env var
  reference will do; the file should be `chmod 600`.
