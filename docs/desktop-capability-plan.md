# 沙箱桌面能力：computer-use 桌面 + MCP 工具 + 串流

创建：2026-11-09。状态：**Phase 1 动工**。目标：给沙箱加上"一台真正的电脑"——
容器里跑 Linux 桌面 + 浏览器，agent 通过 MCP 工具操作它（截图、点击、打字、
导航），用户在浏览器里实时看到桌面。对标 [box by Simular](https://box.simular.ai)。

## 为什么难度不大

box 的四个组成部分，SandrPod 已经有三个半：

| 组成 | box | SandrPod 现状 | 缺口 |
| --- | --- | --- | --- |
| 云端桌面容器 | 每账号一个 Linux 桌面 | 容器运行时（poder，秒级启动） | 桌面镜像（成熟方案，不发明） |
| 电脑操作工具 | 一个 MCP server | **MCP bridge 已上线**（热重载 + OAuth + 控制台标签页） | desktop 工具组（xdotool/scrot 封装） |
| 用户看桌面 | 门户串流 | **preview URL 已通**（`8080-{沙箱}.{域名}` 直达沙箱端口） | KasmVNC 入口 + WS 穿透实测 |
| 睡眠/唤醒/计费 | 闲置睡眠 | 闲置回收 + worker key + 体验节点限额 | 桌面档位单独定价（产品决定） |

最难的部分（MCP 基建、OAuth、穿透链、控制台集成）上一轮已经铺完。

## 架构

```text
agent（Claude / Cursor / 我们的循环）
  │ MCP
  ▼
sandbox bridge /mcp
  └─ desktop 工具组（本方案新增）
       │ subprocess
       ▼
Xvfb + XFCE + Chromium（桌面容器，KasmVNC :6901 供浏览器观看）
```

两个入口：
- **agent 入口**：MCP 工具（computer-use）
- **人入口**：preview URL `https://6901-{sandbox}.{域名}` 打开 KasmVNC 网页客户端

## Phase 1：桌面镜像 + 串流 + 控制台入口（本阶段）

| 项 | 方案 | 验收 |
| --- | --- | --- |
| 桌面镜像 | `docker/Dockerfile.desktop`：alpine 基座不合适（桌面要 glibc），用 Ubuntu 22.04 + Xfce + Chromium + KasmVNC + Noto CJK + ttf 字体；中文 locale。 toolbox 的执行/文件 API **不在**桌面镜像里——桌面镜像只管桌面，执行仍走配套 toolbox 沙箱？**不**：桌面即沙箱，镜像内置精简 toolbox（同一个二进制），保持 execute/files/MCP 全能力 | 容器起一个，浏览器能打开 KasmVNC 页面并进入桌面 |
| 串流穿透 | KasmVNC 自带 HTTPS/WS Web 客户端（:6901）。经 E2B gateway 端口代理 + 隧道，**WS upgrade 必须透传**——实测确认，不通就在 gateway 的 PortProxy 加 upgrade 头转发 | 公网浏览器打开 preview URL 能操作桌面 |
| 控制台入口 | 沙箱详情/MCP 标签页加"打开桌面"按钮 → preview URL；桌面镜像的沙箱自动注册 desktop MCP 工具 | 控制台一键进桌面 |
| 出网策略 | KasmVNC 会话与 Chromium 注入代理 env（白名单照常生效） | 白名单外域名打不开 |

## Phase 2：computer-use MCP 工具组

工具面（第一版，全部 subprocess 封装，不引框架）：

| 工具 | 实现 | 说明 |
| --- | --- | --- |
| `desktop_screenshot` | scrot / GNOME screenshot → PNG（base64 返回） | agent 视觉输入 |
| `desktop_click` | xdotool mousemove+x+y click | 坐标来自 screenshot |
| `desktop_type` | xdotool type（中文走 xdotool type --clearmodifiers + 剪贴板/ibus 后续评估） | |
| `desktop_key` | xdotool key（Return/Tab/ctrl-c…） | |
| `desktop_scroll` | xdotool click 4/5 | |
| `browser_navigate` / `browser_read` | Playwright CDP 连 Chromium：导航、读 DOM 文本（比纯视觉快准） | 双通道：视觉 + DOM |
| `run_command` | 已有（toolbox execute） | 不重复实现 |

### 打断与安全

- 工具调用带 timeout（默认 15s，可调）；执行中第二个调用排队或拒绝（xdotool 加锁）。
- 桌面里的操作**不额外过权限门**（沙箱内信任域同 bridge），agent 模式的权限门照常覆盖 execute 类。
- 出网策略经代理 env 注入桌面会话与 Chromium；策略外的流量拒绝并记审计。

## Phase 3：打磨

中文输入（剪贴板粘贴法 / ibus）、剪贴板互通、分辨率自适应、音频（可选，PulseAudio over WS）、会话录制（审计回放）。

## 成本与密度

桌面容器 RAM 预算 600-900MB（X + WM + Chromium），CPU 峰值在渲染与编码。
一台 4C8G 宿主 **4-10 个桌面沙箱**（无头沙箱几十个）——桌面包是独立档位：
- 镜像独立 tag（`sandrpod-desktop:latest`），按需创建，不进默认调度池
- 定价与配额产品侧另定；体验节点可放一个桌面沙箱作演示

## 验收清单（Phase 1）

- [ ] `docker run sandrpod-desktop` 本机起，浏览器 `localhost:6901` 进桌面
- [ ]preview URL 经 gateway+隧道从公网打开，WS 不断
- [ ] Chromium 出网走代理，白名单外拒绝
- [ ] 控制台沙箱详情出现「打开桌面」
- [ ] MCP manifest 列出 desktop 工具组

## 参考

- [KasmVNC](https://github.com/kasmtech/KasmVNC)：Web 原生 VNC，TCP 友好，穿透简单
- [box by Simular](https://box.simular.ai)：对标产品（桌面 + 托管 MCP + OAuth）
- Playwright CDP：DOM 通道
- xdotool：X 输入注入
