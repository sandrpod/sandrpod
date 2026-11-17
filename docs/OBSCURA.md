# Obscura browser MCP (headless variant)

> **Status**: shipped. Updated: 2026-11

`registry.sandbox.sandrpod.com/sandrpod/toolbox-obscura:latest` is a headless
(no-desktop) toolbox image with the [Obscura](https://github.com/h4ckf0r0day/obscura)
browser engine and its MCP server baked in. Obscura is a Rust headless browser built
for scraping and agent automation (Apache-2.0): V8 for real JavaScript, Chrome
DevTools Protocol, Puppeteer/Playwright compatible, no Chromium required.

Inside the sandbox it registers as a **stdio MCP server**, which the toolbox's MCP
bridge spawns on demand and aggregates into the sandbox's MCP endpoint — so an
agent gets browser tools as plain tool calls.

## What you get

`tools/list` on such a sandbox returns the 5 desktop/sandbox tools plus **38
obscura browser tools** (prefixed `obscura__`):

- `browser_navigate`, `browser_back`, `browser_forward`
- `browser_snapshot` (accessibility tree), `browser_screenshot`, `browser_pdf`
- `browser_click`, `browser_fill`, `browser_fill_form`, `browser_type`
- `browser_extract`, `browser_evaluate` (run JS), `browser_get_cookies`,
  `browser_clear_cookies`, `browser_console_messages`, …

## Using it

Create the sandbox with the variant image (console 沙箱类型 or API `image_id`),
then just talk MCP — no extra setup:

```bash
# tools/list through the sandbox's MCP endpoint
curl -s $API/api/v1/sandboxes/<name>/toolbox/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'

# a session needs the standard MCP initialize dance first
# (initialize → Mcp-Session-Id header → notifications/initialized → tools/*)
```

`browser_navigate` + `browser_screenshot` returns a PNG as an MCP image content
block, so the calling agent sees the page directly.

## Which build

Official releases ship four linux variants. This image uses the **stealth**
x86_64 build (anti-fingerprinting; screenshots/PDF included — the render
pipeline is native Rust, no Chromium, no font packages needed).

```bash
obscura-x86_64-linux-stealth.tar.gz   # what the image bakes
#其它变体: no-render（更小，无截图/PDF）、non-stealth（无反检测）
```

## Anti-bot sites

Navigation and rendering work on normal sites out of the box. Sites with
datacenter-IP challenges (PyPI's "Client Challenge", Cloudflare-protected pages)
block by **IP reputation**, which no browser fingerprint change fixes. Options:

- point obscura at a proxy: `OBSCURA_PROXY` / the `--proxy` flag (residential
  proxies are what actually pass those checks), or
- route the sandbox's egress through one via the egress policy + your own proxy.

## Image recipe

`docker/Dockerfile.toolbox-obscura` (in the engine repo):

- base `debian:bookworm-slim` — the official obscura binaries are glibc builds and
  the standard toolbox image is Alpine/musl, which cannot execute them;
- the toolbox binary is CGO-free and static, so it moves over unchanged;
- the default `mcp.json` is baked at `/root/.sandrpod/mcp.json` (the path the MCP
  bridge reads); the workspace-volume boot logic then keeps it across rebuilds.

## Notes

- The obscura **HTTP** transport (`obscura mcp --http --host 0.0.0.0`, bearer
  token ≥ 32 chars) is for sidecar deployments; the baked default is stdio,
  which needs no ports and no token.
- Upstream moves fast (v0.2.7 at time of writing); bump the pinned version in
  the Dockerfile to pick up engine fixes.
