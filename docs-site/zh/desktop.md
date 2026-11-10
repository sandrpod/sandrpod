# Desktop sandboxes

> A desktop sandbox is a normal SandrPod sandbox running an image with XFCE, Google
> Chrome and the toolbox. Watch it in the browser, drive it with a mouse, or let an
> agent operate it over CDP.

## Pick the tier when you create

| Type in the console | Image | Screen |
|---|---|---|
| 桌面沙箱 / Desktop | `sandrpod-desktop:v6` | 1280×800 |
| 桌面沙箱 · 流畅 / Desktop (light) | `sandrpod-desktop:v6s` | 1024×640, ~40% fewer pixels |

Via the API it is just `image_id`:

```bash
curl -s $API/api/v1/sandboxes -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"desk","image_id":"sandrpod-desktop:v6","cpu_cores":2,"memory_mb":4096}'
```

A desktop needs **2 vCPU / 4 GiB**: X, XFCE, Chrome and the H.264 encoder together
overrun 1 GiB and the container gets OOM-killed on restart. The console raises the
defaults when you pick a desktop type.

## Three ways in

| Entry | URL | Who it is for |
|---|---|---|
| Viewer | `https://6901-<sandbox>.<preview-domain>/` | A human watching or driving the screen |
| CDP | `https://9222-<sandbox>.<preview-domain>/json/version` | Playwright / chrome-devtools-mcp driving **the same** Chrome |
| Sandbox MCP | `/api/v1/sandboxes/<name>/toolbox/mcp` → `desktop_*` | An agent that wants screenshot/click/type as tool calls |

The console's row button **桌面** mints a signed URL and opens the viewer.

## Preview links are signed

The wildcard host refuses an unsigned request — it exposes a desktop, not a web page.

- `GET /api/v1/sandboxes/{name}/preview/{port}` returns an HMAC-signed URL bound to
  sandbox + port + expiry (2 hours).
- Opening it sets an HttpOnly cookie, which the page's subresources and its
  **websocket** then reuse (a browser cannot attach headers to a WS handshake).
- A platform API key in `Authorization` works too, so `curl` and the E2B SDK are
  unaffected.

## Persistence

Stopping a desktop sandbox **releases CPU, memory and GPUs** but keeps the data:

```
/root/Desktop          → /workspace/.home/Desktop
/root/Downloads        → /workspace/.home/Downloads
/root/Documents        → /workspace/.home/Documents
/root/.chrome-profile  → /workspace/.home/chrome-profile
```

So files, downloads, Chrome tabs and logins survive a stop **and** an image upgrade.
`/root/.config` is deliberately not persisted: the theme and panel layout belong to
the image.

## Streaming quality

The image ships H.264 (`-videoCodec h264`) plus the ffmpeg runtime and the
unversioned `libav*.so` sonames KasmVNC `dlopen`s — without those it silently falls
back to webp at roughly ten times the CPU. Frame rate is capped at 30.

Measured: the encoder uses ~0.2–3% of a 4-core host while streaming, so bandwidth is
the limit, not compute. 10 Mbps carries the 1280×800 stream; the light tier is for
when it does not.

## Driving Chrome over CDP

Chrome starts with `--remote-debugging-port=9222 --remote-debugging-address=0.0.0.0
--remote-allow-origins=*`, and the desktop auto-opens a window, so the endpoint is
live as soon as the sandbox is.

```bash
curl -s https://9222-<sandbox>.<domain>/json/version
curl -s https://9222-<sandbox>.<domain>/json/list    # page ids
```

Chrome reports `webSocketDebuggerUrl` with `127.0.0.1:9222` (the address it bound);
rewrite the host, keep the path:

```
wss://9222-<sandbox>.<domain>/devtools/page/<pageId>
```

```json
{ "mcpServers": { "playwright": { "command": "npx",
  "args": ["@playwright/mcp@latest", "--cdp-endpoint",
           "wss://9222-<sandbox>.<domain>/devtools/page/<pageId>"] } } }
```

Because it is the sandbox's real Chrome, a person can take over the same window
mid-task.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Viewer stuck at 连接中 | A proxy in the path dropped the websocket upgrade |
| `401 preview requires a signed URL token` | Open it from the console; the raw URL is not public |
| `No password configured for VNC Auth` | X started without `-SecurityTypes None` |
| Container dies right after `start` | Less than 2 GiB of memory, or a stale X/dbus state (the entrypoint clears it) |
