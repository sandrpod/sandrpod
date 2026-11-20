# Obscura 浏览器 MCP（无桌面）

> 一个内置 [Obscura](https://github.com/h4ckf0r0day/obscura) 浏览器引擎及其 MCP
> server 的无桌面 toolbox 镜像——浏览器自动化以普通工具调用的形式提供，无需桌面。

`registry.sandbox.sandrpod.com/sandrpod/toolbox-obscura:latest` 是一个无桌面
（不带桌面栈）的沙箱镜像，内置 Obscura。Obscura 是一个用 Rust 编写的无头浏览器，
面向爬虫和 agent 自动化（Apache-2.0）：内置 V8 执行真实 JavaScript、支持 Chrome
DevTools Protocol、兼容 Puppeteer/Playwright，不需要 Chromium。

在沙箱内部，它以 **stdio MCP server** 的形式注册，由 toolbox 的 MCP bridge 按需
拉起并聚合进沙箱的 MCP 端点。

## 提供什么

这类沙箱的 `tools/list` 会返回 5 个沙箱工具加 **38 个 obscura 浏览器工具**
（前缀 `obscura__`）：

- `browser_navigate`、`browser_back`、`browser_forward`
- `browser_snapshot`（无障碍树）、`browser_screenshot`、`browser_pdf`
- `browser_click`、`browser_fill`、`browser_fill_form`、`browser_type`
- `browser_extract`、`browser_evaluate`（执行 JS）、`browser_get_cookies`、
  `browser_clear_cookies`、`browser_console_messages`、…

## 怎么用

创建沙箱时选这个变体镜像（API 的 `image_id`），然后直接说 MCP——一个会话需要
先走标准的 MCP initialize 握手：

```bash
# 1. initialize → Mcp-Session-Id 响应头
# 2. notifications/initialized
# 3. tools/list、tools/call —— 每次调用都带 Mcp-Session-Id
curl -s $API/api/v1/sandboxes/<name>/toolbox/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"obscura__browser_navigate",
       "arguments":{"url":"https://example.com"}}}'
```

`browser_navigate` + `browser_screenshot` 会返回 PNG 形式的 MCP image content，
调用方 agent 能直接看到页面。

## 用哪个构建

官方 release 提供四种 linux 变体。本镜像使用 **stealth** x86_64 构建
（反浏览器指纹；包含截图/PDF——渲染管线是 Rust 原生实现，不需要 Chromium）。

镜像内另装 `fontconfig` + `fonts-noto-cjk`，并以
`--font-dir /usr/share/fonts/opentype/noto` 启动 Obscura：Obscura 不会自行加载
系统字体，不装的话任何中文页面都是方框。

## 已知限制：原生表单控件里的文字

正文的中文渲染正常，但 **`<input>` 的 value**（submit / reset / text）、
`<select>` 和 placeholder 由 Obscura 内置的拉丁字体绘制，没有逐字形回退，
所以 `Google 搜尋` / `好手氣` 这类按钮里的中文，无论装什么字体都是方框。
`<button>` 的文字和普通正文不受影响。这是上游
[issue #606](https://github.com/h4ckf0r0day/obscura/issues/606)（未修复；
PR #991 是尚未合并的补丁）。绕行办法：自己控制的页面用 `<button>`；需要按钮
文案保真时用桌面档位（真 Chrome）。

## 反爬站点

普通网站的导航和渲染开箱即用。带数据中心 IP 挑战的站点（PyPI 的
"Client Challenge"、Cloudflare 防护页）是按 **IP 信誉**拦截的，换浏览器指纹
没用。可选方案：给 obscura 配代理（`OBSCURA_PROXY` / `--proxy` 参数——住宅代理
才能真正通过这类检查），或通过出网策略把沙箱出口流量导向你自己的代理。
