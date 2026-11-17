# Desktop sandboxes

> **Status**: shipped. Updated: 2026-11

A desktop sandbox is a normal SandrPod sandbox running an image with a full
XFCE desktop, Google Chrome and the toolbox. It is watched and driven in the
browser over KasmVNC, and it can also be piloted by an agent through CDP or the
desktop MCP tools.

## Images

Five registry-built variants, all from `docker/Dockerfile.desktop` (the CI matrix in
`.gitea/workflows/registry-images.yml` builds and pushes them on every main push):

| Console type | Image | Look |
|---|---|---|
| 桌面沙箱 / Desktop | `sandrpod/desktop:latest` | Stock Ubuntu XFCE + Chrome |
| 桌面沙箱 · 精简 | `sandrpod/desktop-small:latest` | Same, 1024×640, for a capped uplink |
| 桌面沙箱 · 素净 | `sandrpod/desktop-plain:latest` | Stock XFCE without the Ubuntu theme |
| 桌面沙箱 · 底部 Dock | `sandrpod/desktop-dock:latest` | Bottom dock, no top panel |
| 桌面沙箱 · mac 风 | `sandrpod/desktop-mac:latest` | WhiteSur (light) theme + icons + Inter font + plank dock |

The older local tags `sandrpod-desktop:v6` / `v6s` still work.

Sandbox records carry an explicit `desktop: true` flag set at create time — the
console no longer guesses the tier from the image name, so a custom desktop image
keeps the desktop buttons.

### The mac variant

`desktop-mac` is the closest to a macOS desktop: WhiteSur light GTK theme, WhiteSur
icons, Inter font, an xfwm4 skin, and **plank** as the only dock (the XFCE panel is
disabled in this image). Plank auto-centers and follows the browser window size —
KasmVNC resizes the virtual screen to the viewer, and a panel cannot do that.

Wallpaper is the stock one; change it per-sandbox (right-click → Desktop Settings) if
you want the WhiteSur art. Egress-restricted desktops need `-publicIP` on Xvnc (the
image pins it) or KasmVNC's STUN probe fails on the isolated network and the desktop
never starts.

Interactive terminals (console 终端, PTY) start in **/workspace**, same as
`sandbox_exec`.

## Three ways in

| Entry | URL | Who it is for |
|---|---|---|
| Viewer (KasmVNC) | `https://6901-<sandbox>.<preview-domain>/` | A human watching or driving the screen |
| CDP | `https://9222-<sandbox>.<preview-domain>/json/version` (signed token or API key) | Playwright MCP / chrome-devtools-mcp driving **the same** Chrome |
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

**CDP is a preview port like 6901: it needs a signed token or an API key.** A bare
URL answers `401 preview requires a signed URL token`; it is a credential for the
sandbox's real browser, not a public endpoint.

```bash
# API key:
curl -s -H "Authorization: Bearer $SANDRPOD_API_TOKEN" \
  https://9222-<sandbox>.<domain>/json/version
curl -s -H "Authorization: Bearer $SANDRPOD_API_TOKEN" \
  https://9222-<sandbox>.<domain>/json/list      # page ids

# Or the signed URL the console uses (2h, bound to sandbox + port):
URL=$(curl -s -H "Authorization: Bearer $SANDRPOD_API_TOKEN" \
  https://cloud.example.com/api/v1/sandboxes/<sandbox>/preview/9222 | jq -r .url)
TOKEN=${URL##*token=}
curl -s "https://9222-<sandbox>.<domain>/json/version?token=$TOKEN"
```

Chrome reports `webSocketDebuggerUrl` with `127.0.0.1:9222` as the host (that is
the address it bound); a remote client rewrites the host, keeps the path, and
carries the token:

```
wss://9222-<sandbox>.<domain>/devtools/page/<pageId>?token=<signed>
```

Playwright MCP:

```json
{ "mcpServers": { "playwright": { "command": "npx",
  "args": ["@playwright/mcp@latest", "--cdp-endpoint",
           "wss://9222-<sandbox>.<domain>/devtools/page/<pageId>?token=<signed>"] } } }
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
| Terminal opens in `/` | Toolbox older than 2026-11-15 — rebuild the image (the PTY now starts in /workspace) |
| Egress-restricted desktop never starts, Xvnc log full of `STUN server didn't work` | KasmVNC's ICE probe cannot leave the isolated network; the image pins `-publicIP` |
