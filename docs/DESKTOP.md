# Desktop sandboxes

> **Status**: shipped. Updated: 2026-11

A desktop sandbox is a normal SandrPod sandbox running an image with a full
XFCE desktop, Google Chrome and the toolbox. It is watched and driven in the
browser over KasmVNC, and it can also be piloted by an agent through CDP or the
desktop MCP tools.

## Images

| Tag | Screen | Use |
|---|---|---|
| `sandrpod-desktop:v6` | 1280×800 | Standard |
| `sandrpod-desktop:v6s` | 1024×640 | ~40% fewer pixels; for a capped uplink |

Both are built from `docker/Dockerfile.desktop`; the only difference is
`--build-arg DESKTOP_GEOMETRY=1024x640`.

## Three ways in

| Entry | URL | Who it is for |
|---|---|---|
| Viewer (KasmVNC) | `https://6901-<sandbox>.<preview-domain>/` | A human watching or driving the screen |
| CDP | `https://9222-<sandbox>.<preview-domain>/json/version` | Playwright MCP / chrome-devtools-mcp driving **the same** Chrome |
| Sandbox MCP | `/api/v1/sandboxes/<name>/toolbox/mcp` → `desktop_*` tools | An agent that wants screenshot/click/type as tool calls |

The console's sandbox detail has an **打开桌面** button that mints a signed URL
for the viewer.

## Preview authentication

The wildcard preview host refuses requests without a credential, because it
exposes a stranger's desktop otherwise.

- `GET /api/v1/sandboxes/{name}/preview/{port}` mints an HMAC-signed URL bound to
  sandbox + port + expiry (2h).
- A valid `?token=` sets an HttpOnly cookie, which the page's subresources and
  its websocket handshake then reuse — a browser cannot add headers to a
  websocket.
- A platform API key in `Authorization` is equally accepted, so `curl` and the
  E2B SDK work unchanged.

Signing key: `SANDRPOD_PREVIEW_SECRET`, else derived from the platform token.
With authentication disabled (self-hosted dev) previews stay open like
everything else.

## Driving Chrome over CDP

Chrome starts with `--remote-debugging-port=9222 --remote-debugging-address=0.0.0.0
--remote-allow-origins=*`, and the desktop auto-opens a window so the endpoint
is live as soon as the sandbox is.

```bash
curl -s https://9222-<sandbox>.<domain>/json/version
curl -s https://9222-<sandbox>.<domain>/json/list      # page ids
```

Chrome reports `webSocketDebuggerUrl` with `127.0.0.1:9222` as the host (that is
the address it bound); a remote client rewrites the host and keeps the path:

```
wss://9222-<sandbox>.<domain>/devtools/page/<pageId>
```

Playwright MCP:

```json
{ "mcpServers": { "playwright": { "command": "npx",
  "args": ["@playwright/mcp@latest", "--cdp-endpoint",
           "wss://9222-<sandbox>.<domain>/devtools/page/<pageId>"] } } }
```

Because it is the sandbox's real Chrome, a human can take over the same window
mid-task.

## Persistence

An idle desktop sandbox is **stopped**, not deleted: CPU and memory are released,
the container and its workspace survive, and `start` resumes it.

User data lives on the `/workspace` volume:

```
/root/Desktop          → /workspace/.home/Desktop
/root/Downloads        → /workspace/.home/Downloads
/root/Documents        → /workspace/.home/Documents
/root/.chrome-profile  → /workspace/.home/chrome-profile
/root/.sandrpod        → /workspace/.home/.sandrpod   (mcp.json + OAuth 令牌)
```

so files, downloads, Chrome tabs and logins survive a stop — and an image
upgrade, because they are on a volume rather than in the container.

`/root/.config` is deliberately **not** persisted: the Yaru theme, wallpaper and
panel layout are baked into the image, and persisting them would freeze the old
look across upgrades.

## Streaming performance

Two settings matter, both already in the image:

- **H.264** (`-videoCodec h264`) with the ffmpeg runtime present — the webp
  fallback costs an order of magnitude more CPU. The image also creates the
  unversioned `libav*.so` sonames KasmVNC `dlopen`s; without them H.264 is never
  selected.
- **30 fps** (`-FrameRate 30`) — 60 is pure encoder burn on a remote desktop.

Measured on a 4C8G host: the encoder sits at ~0.2–3% CPU while streaming, so the
limit is bandwidth, not compute. A 10 Mbps uplink carries the 1280×800 stream
comfortably; the 1024×640 variant is for when it does not.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Viewer stuck at 连接中 | Websocket upgrade dropped by an intermediate proxy — every hop must relay raw bytes (`proxyHTTPStreaming` / `proxyUpgradeTo` do) |
| `401 preview requires a signed URL token` | Open it from the console or use `/preview/{port}`; the raw URL is not public |
| `No password configured for VNC Auth` | The X server was started without `-SecurityTypes None` |
| Blank Chrome window, log full of `incorrect profile type` | Chrome needs `--user-data-dir` (the image sets `/root/.chrome-profile`) |
