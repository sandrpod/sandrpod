# Obscura 浏览器 MCP（无桌面变体）

> **状态**：已交付。更新：2026-11

`registry.sandbox.sandrpod.com/sandrpod/toolbox-obscura:latest` 是一个无桌面
（不带桌面栈）的 toolbox 镜像，内置 [Obscura](https://github.com/h4ckf0r0day/obscura)
浏览器引擎及其 MCP server。Obscura 是一个用 Rust 编写的无头浏览器，面向爬虫和
agent 自动化（Apache-2.0）：内置 V8 执行真实 JavaScript、支持 Chrome DevTools
Protocol、兼容 Puppeteer/Playwright，不需要 Chromium。

在沙箱内部，它以 **stdio MCP server** 的形式注册，由 toolbox 的 MCP bridge 按需
拉起并聚合进沙箱的 MCP 端点——agent 拿到的就是普通的工具调用。

## 提供什么

这类沙箱的 `tools/list` 会返回 5 个桌面/沙箱工具加 **38 个 obscura 浏览器工具**
（前缀 `obscura__`）：

- `browser_navigate`、`browser_back`、`browser_forward`
- `browser_snapshot`（无障碍树）、`browser_screenshot`、`browser_pdf`
- `browser_click`、`browser_fill`、`browser_fill_form`、`browser_type`
- `browser_extract`、`browser_evaluate`（执行 JS）、`browser_get_cookies`、
  `browser_clear_cookies`、`browser_console_messages`、…

## 怎么用

创建沙箱时选这个变体镜像（控制台沙箱类型或 API 的 `image_id`），然后直接说
MCP——不需要额外设置：

```bash
# 通过沙箱 MCP 端点列工具
curl -s $API/api/v1/sandboxes/<name>/toolbox/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'

# 一个会话需要先走标准的 MCP initialize 握手
# （initialize → Mcp-Session-Id 响应头 → notifications/initialized → tools/*）
```

`browser_navigate` + `browser_screenshot` 会返回 PNG 形式的 MCP image content，
调用方 agent 能直接看到页面。

## 用哪个构建

官方 release 提供四种 linux 变体。本镜像使用 **stealth** x86_64 构建
（反浏览器指纹；包含截图/PDF——渲染管线是 Rust 原生实现，无需 Chromium、
无需字体包）。

```bash
obscura-x86_64-linux-stealth.tar.gz   # 镜像内烘焙的就是它
#其它变体: no-render（更小，无截图/PDF）、non-stealth（无反检测）
```

## 反爬站点

普通网站的导航和渲染开箱即用。带数据中心 IP 挑战的站点（PyPI 的
"Client Challenge"、Cloudflare 防护页）是按 **IP 信誉**拦截的，换浏览器指纹
没用。可选方案：

- 给 obscura 配代理：`OBSCURA_PROXY` / `--proxy` 参数（住宅代理才能真正通过
  这类检查），或
- 通过出网策略把沙箱出口流量导向你自己的代理。

## 镜像配方

`docker/Dockerfile.toolbox-obscura`（引擎仓库内）：

- 基座 `debian:bookworm-slim` —— 官方 obscura 二进制是 glibc 构建，而标准
  toolbox 镜像是 Alpine/musl，无法执行；
- toolbox 二进制是 CGO-free 的静态构建，跨基座搬移无损；
- 默认 `mcp.json` 烘焙在 `/root/.sandrpod/mcp.json`（MCP bridge 读取的路径），
  开机时的 workspace 搬移逻辑会把它保留到数据卷上，跨镜像升级不丢。

## 备注

- obscura 的 **HTTP** 传输（`obscura mcp --http --host 0.0.0.0`，bearer token
  ≥ 32 字符）用于 sidecar 部署；镜像默认烘焙的是 stdio，不需要端口也不需要
  token。
- 上游迭代很快（写作时为 v0.2.7）；升级 Dockerfile 里固定的版本即可获得
  引擎修复。
