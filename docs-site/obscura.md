# Obscura browser MCP (headless)

> A headless toolbox image with the [Obscura](https://github.com/h4ckf0r0day/obscura)
> browser engine and its MCP server baked in — browser automation as plain tool
> calls, no desktop required.

`registry.sandbox.sandrpod.com/sandrpod/toolbox-obscura:latest` is a headless
(no-desktop) sandbox image with Obscura inside. Obscura is a Rust headless browser
built for scraping and agent automation (Apache-2.0): V8 for real JavaScript, Chrome
DevTools Protocol, Puppeteer/Playwright compatible, no Chromium required.

Inside the sandbox it registers as a **stdio MCP server**, which the toolbox's MCP
bridge spawns on demand and aggregates into the sandbox's MCP endpoint.

## What you get

`tools/list` on such a sandbox returns the 5 sandbox tools plus **38 obscura
browser tools** (prefixed `obscura__`):

- `browser_navigate`, `browser_back`, `browser_forward`
- `browser_snapshot` (accessibility tree), `browser_screenshot`, `browser_pdf`
- `browser_click`, `browser_fill`, `browser_fill_form`, `browser_type`
- `browser_extract`, `browser_evaluate` (run JS), `browser_get_cookies`,
  `browser_clear_cookies`, `browser_console_messages`, …

## Using it

Create the sandbox with the variant image (API `image_id`), then talk MCP — the
session needs the standard MCP initialize dance first:

```bash
# 1. initialize → Mcp-Session-Id response header
# 2. notifications/initialized
# 3. tools/list, tools/call — with Mcp-Session-Id on every call
curl -s $API/api/v1/sandboxes/<name>/toolbox/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"obscura__browser_navigate",
       "arguments":{"url":"https://example.com"}}}'
```

`browser_navigate` + `browser_screenshot` returns a PNG as an MCP image content
block, so the calling agent sees the page directly.

## Which build

Official releases ship four linux variants. This image uses the **stealth**
x86_64 build (anti-fingerprinting; screenshots/PDF included — the render
pipeline is native Rust, no Chromium required).

The image also ships `fontconfig` + `fonts-noto-cjk` and starts Obscura with
`--font-dir /usr/share/fonts/opentype/noto`: Obscura does not load system
fonts on its own, so without this every CJK page renders as tofu boxes.

## Known limitation: text inside native form controls

Paragraph text renders CJK correctly, but the **value of an `<input>`**
(submit / reset / text), `<select>` and placeholders are painted from Obscura's
bundled Latin faces without per-glyph fallback, so labels like `Google 搜尋` /
`好手氣` stay tofu no matter which fonts are installed. `<button>` text and
normal text are unaffected. This is upstream
[issue #606](https://github.com/h4ckf0r0day/obscura/issues/606) (open; PR #991
is the unmerged fix). Workarounds: use `<button>` on pages you control, or the
desktop tier (real Chrome) when control-label fidelity matters.

## Anti-bot sites

Navigation and rendering work on normal sites out of the box. Sites with
datacenter-IP challenges (PyPI's "Client Challenge", Cloudflare-protected pages)
block by **IP reputation**, which no browser fingerprint change fixes. Point
obscura at a proxy (`OBSCURA_PROXY` / the `--proxy` flag) — residential proxies
are what actually pass those checks — or route the sandbox's egress through one
via the egress policy.
