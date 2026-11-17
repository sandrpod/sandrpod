# 桌面沙箱

> 桌面沙箱就是一个普通 SandrPod 沙箱，跑的是带 XFCE、Google Chrome 和 toolbox 的镜像。
> 可以在浏览器里观看、用鼠标操作，也可以让 agent 通过 CDP 驱动它。

## 创建时选档位

| 控制台里的类型 | 镜像 | 外观 |
|---|---|---|
| 桌面沙箱 | `sandrpod/desktop:latest` | 原生 Ubuntu XFCE + Chrome，1280×800 |
| 桌面沙箱 · 精简 | `sandrpod/desktop-small:latest` | 1024×640，像素少约 40% |
| 桌面沙箱 · 素净 | `sandrpod/desktop-plain:latest` | 原生 XFCE，无 Ubuntu 主题 |
| 桌面沙箱 · 底部 Dock | `sandrpod/desktop-dock:latest` | 底部 dock，无顶栏 |
| 桌面沙箱 · mac 风 | `sandrpod/desktop-mac:latest` | WhiteSur 浅色主题 + 图标，Inter 字体，plank dock |

```bash
curl -s $API/api/v1/sandboxes -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"desk","image_id":"sandrpod/desktop-mac:latest","desktop":true,"cpu_cores":2,"memory_mb":4096}'
```

沙箱记录里有显式的 `desktop: true` 标记——控制台不再按镜像名猜类型，自定义桌面
镜像也能保留桌面按钮。

### 无桌面 + 浏览器 MCP

不需要桌面、只要浏览器自动化工具的话，
[`sandrpod/toolbox-obscura:latest`](https://github.com/h4ckf0r0day/obscura) 是
内置 Obscura 浏览器引擎及其 MCP server 的无桌面 toolbox——38 个浏览器工具
（`navigate`、`screenshot`、`click`、`extract`、`evaluate`…）通过沙箱的 MCP
端点暴露。详见 [Obscura 浏览器 MCP](/zh/obscura)。

桌面需要 **2 vCPU / 4 GiB**：X、XFCE、Chrome 加 H.264 编码器一起会吃满 1 GiB，容器在重启时会被 OOM 杀掉。控制台在选桌面类型时会自动把默认值提上来。

## 三种入口

| 入口 | 地址 | 给谁用 |
|---|---|---|
| 观看端 | `https://6901-<沙箱>.<预览域>/` | 用眼睛看、用鼠标操作的人 |
| CDP | `https://9222-<沙箱>.<预览域>/json/version`（需签名 token 或 API Key） | Playwright / chrome-devtools-mcp 驱动**同一个** Chrome |
| 沙箱 MCP | `/api/v1/sandboxes/<名>/toolbox/mcp` → `desktop_*` | 想要"截图/点击/输入"作为工具调用的 agent |

控制台里的 **桌面** 按钮会签发一个签名 URL 并打开观看端。

## 预览链接是签名的

通配主机拒绝未签名的请求——它暴露的是一块桌面，不是网页。

- `GET /api/v1/sandboxes/{名}/preview/{端口}` 返回绑定"沙箱 + 端口 + 有效期"（2 小时）的 HMAC 签名 URL。
- 打开它会让页面拿到一个 HttpOnly cookie，页面的子资源和它的 **websocket** 随后都复用它（浏览器无法给 WS 握手加头）。
- `Authorization` 里放平台 API key 同样被接受，所以 `curl` 和 E2B SDK 不受影响。

## 数据持久化

停止桌面沙箱会**释放 CPU、内存和 GPU**，但保留数据：

```
/root/Desktop          → /workspace/.home/Desktop
/root/Downloads        → /workspace/.home/Downloads
/root/Documents        → /workspace/.home/Documents
/root/.chrome-profile  → /workspace/.home/chrome-profile
/root/.sandrpod        → /workspace/.home/.sandrpod   (mcp.json + OAuth 令牌)
```

所以文件、下载、Chrome 标签页和登录态都能活过一次停止**以及**一次镜像升级。
`/root/.config` 是特意不持久化的：主题和面板布局属于镜像。

## 流媒体质量

镜像里带 H.264（`-videoCodec h264`）加 ffmpeg 运行时，以及 KasmVNC `dlopen` 需要的
无版本号 `libav*.so`——缺了这些它会静默回退到 webp，CPU 开销大约十倍。帧率上限 30。

实测：一台 4 核主机上编码器只占 0.2–3%，所以瓶颈是带宽不是算力。10 Mbps 足够承载
1280×800 的流；承载不了时才需要"流畅"档。

## 用 CDP 驱动 Chrome

Chrome 以 `--remote-debugging-port=9222 --remote-debugging-address=0.0.0.0
--remote-allow-origins=*` 启动，桌面会自动开一个窗口，所以沙箱一起来这个端点就活了。

**CDP 和 6901 一样是预览端口：必须有签名 token 或 API Key。** 裸 URL 会返回
`401 preview requires a signed URL token`——它是沙箱里真实浏览器的凭证，不是公开端口。

```bash
# 用 API Key：
curl -s -H "Authorization: Bearer $SANDRPOD_API_TOKEN" \
  https://9222-<沙箱>.<域名>/json/version
curl -s -H "Authorization: Bearer $SANDRPOD_API_TOKEN" \
  https://9222-<沙箱>.<域名>/json/list    # 页面 id

# 或者签一个和控制台相同的 URL（2 小时，绑定沙箱 + 端口）：
URL=$(curl -s -H "Authorization: Bearer $SANDRPOD_API_TOKEN" \
  https://cloud.example.com/api/v1/sandboxes/<沙箱>/preview/9222 | jq -r .url)
TOKEN=${URL##*token=}
curl -s "https://9222-<沙箱>.<域名>/json/version?token=$TOKEN"
```

Chrome 上报的 `webSocketDebuggerUrl` 里是 `127.0.0.1:9222`（它绑定的地址）；把主机改掉、路径保留，
token 放在查询串里：

```
wss://9222-<沙箱>.<域名>/devtools/page/<pageId>?token=<签名>
```

```json
{ "mcpServers": { "playwright": { "command": "npx",
  "args": ["@playwright/mcp@latest", "--cdp-endpoint",
           "wss://9222-<沙箱>.<域名>/devtools/page/<pageId>?token=<签名>"] } } }
```

因为它驱动的是沙箱里真实的 Chrome，人可以在任务中途接管同一个窗口。

## 故障排查

| 现象 | 原因 |
|---|---|
| 观看端停在"连接中" | 链路上有代理丢掉了 websocket upgrade |
| `401 preview requires a signed URL token` | 从控制台打开；裸 URL 不是公开的 |
| `No password configured for VNC Auth` | X 启动时没带 `-SecurityTypes None` |
| `start` 之后容器立刻退出 | 内存不足 2 GiB，或者残留的 X/dbus 状态（入口脚本会清理） |
