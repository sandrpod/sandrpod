# MCP 入口与沙箱数据持久化：设计

> 目标：把 SandrPod 的能力以 **MCP** 暴露给 agent 客户端（Claude Desktop / Cursor /
> Cline / 自研 agent），同时让沙箱在**闲置时释放容器**以省资源、**数据不丢**。

## 一、市场能力对照

按能力块而不是按产品看，主流沙箱（E2B / Daytona / Modal / Morph / Browserbase /
Steel / Kasm）的标配：

| 能力块 | 说明 | SandrPod |
|---|---|---|
| 生命周期 | 创建/暂停/销毁、TTL、配额 | ✅ |
| 代码执行 | 无状态 exec + 有状态内核 | ✅ |
| 文件系统 | ls/read/write/upload/download/watch | ✅ |
| Shell/PTY/进程 | 交互终端、后台进程、事件流 | ✅ |
| 网络策略 | 出网白名单、代理、审计 | ✅（差异化） |
| 预览/端口 | `<port>-<sandbox>.<domain>` | ✅ |
| 浏览器 | 无头 Chrome + CDP + live view | ⚠️ 有 Chrome，未开 CDP |
| 桌面（GUI） | 完整桌面 + 键鼠注入 | ✅（差异化） |
| 快照/分叉 | 秒级快照、分支实验 | ❌ |
| GPU | 按需 GPU | ❌ |

**结论**：browser-use MCP 只覆盖「网页自动化」一块，不够；但**浏览器不该自研**——
开 CDP 让 Playwright MCP / chrome-devtools-mcp 接管即可。真正缺的是**把已有能力
以 MCP 暴露**，以及**闲置释放 + 数据持久化**这对组合。

## 二、MCP 设计

### 2.1 为什么做

- MCP 是当前 agent 客户端的标准入口；没有 MCP，用户必须先写 SDK 代码才能用沙箱。
- 所有能力已存在（REST + toolbox），适配层很薄：**协议转换，不重复业务逻辑**。

### 2.2 两种 MCP 端点

**A. 平台级 MCP（控制面 `/mcp`）** —— 面向「给我一个沙箱」的 agent：

| 工具 | 映射 |
|---|---|
| `sandbox_create` / `sandbox_list` / `sandbox_delete` | REST |
| `sandbox_exec` | `POST /sandboxes/execute`（可流式） |
| `sandbox_read_file` / `sandbox_write_file` | toolbox `/files` |
| `sandbox_preview_url` | 端口代理 |
| `sandbox_stop` / `sandbox_start` | 生命周期（持久化感知） |

传输用 Streamable HTTP（`pkg/mcpbridge` 已有实现可复用），认证用现成 API key。

**B. 沙箱内 MCP（toolbox `/mcp`）** —— 现有的 mcpbridge 聚合第三方 MCP 服务；
新增一组**内置工具**（与用户配置的 MCP 服务并列）：

| 工具 | 实现 |
|---|---|
| `desktop_screenshot` | scrot/xwd → PNG |
| `desktop_click` / `desktop_double_click` | xdotool |
| `desktop_type` / `desktop_key` | xdotool |
| `desktop_scroll` | xdotool |
| `browser_open` | 启动 Chrome（含 CDP 端口） |

### 2.3 浏览器：开 CDP，不自研

```
Chrome 启动参数加 --remote-debugging-port=9222
→ 预览链路暴露 https://9222-<sandbox>.<domain>
→ Playwright MCP / chrome-devtools-mcp 以 CDP URL 接入
```

工作量半天，直接获得成熟的浏览器自动化（截图、DOM、点击、导航），比自研 browser-use
更稳。

## 三、数据持久化（本次实现的重点）

### 3.1 问题

沙箱闲置时若直接删除容器，容器内所有文件（用户代码、下载的产物、桌面会话状态）全部
丢失；若为了保数据而不释放，则 4C8G 的主机跑不了几个桌面沙箱就满了。

### 3.2 设计

```
每个沙箱一个 docker volume：sandrpod-ws-<sandbox-name>
挂载点：/workspace

容器生命周期：
  RUNNING ──(闲置 TTL)──▶ STOPPED   docker stop：释放 CPU/内存，容器与卷都在
  STOPPED ──(start)────▶ RUNNING    docker start：秒级恢复，数据原样
  STOPPED ──(保留期到期 / 显式删除)──▶ 删除容器 + 删除卷
```

要点：

1. **数据面**：`/workspace` 是卷，容器重建（镜像升级、重试创建）后数据仍在。
2. **状态面**：容器 `stop`（不是 `pause`）——`pause` 保留内存，等于没省资源；
   `stop` 把进程全部结束，只留磁盘（卷）。
3. **语义面**：区分三个时间
   - `ttl_seconds`（创建时指定）：**闲置多久后释放容器**（进 STOPPED）
   - `SANDRPOD_SANDBOX_RETAIN`（部署级，默认 7d）：STOPPED 后**保留多久再删除**
   - 显式 `DELETE`：立刻删容器 + 卷
4. **恢复面**：`POST /sandboxes/{name}/start` 恢复（docker start，数据在）；
   MCP/exec 对 STOPPED 沙箱**自动恢复**（可选开关）。

### 3.3 桌面沙箱的特殊处理

桌面镜像的用户数据在 `/workspace` + `/root`（Chrome profile、下载、桌面配置）。
第一阶段：`/workspace` 持久化（用户代码/产物），桌面内的应用状态视为临时；
第二阶段：把 `/root/.chrome-profile`、`/root/Desktop`、`/root/Downloads` 也挂到卷内的
子目录（`/workspace/.home/...`），实现「桌面会话也能续」。

### 3.4 与 E2B 兼容性的关系

E2B 的 `timeout` 语义是「沙箱存活时间」；本设计下 `ttl_seconds` 变成「闲置后释放」，
对 E2B 客户端是**更宽**的语义（数据不丢），但 `DELETE` 仍然是彻底销毁。文档需要明确。

## 四、落地顺序

| 阶段 | 内容 | 验收 |
|---|---|---|
| P0-a | 卷挂载 + stop/start + 闲置释放 reaper | 写文件 → 停止 → 启动 → 文件还在 |
| P0-b | 平台级 MCP（exec/files/preview） | Claude Desktop 直连可执行命令 |
| P0-c | 桌面 Chrome 开 CDP | Playwright MCP 可截图操作 |
| P1 | 桌面 MCP 工具（screenshot/click/type） | agent 点开应用完成一次操作 |
| P1 | 桌面 home 持久化 | 重启后 Chrome 标签/下载还在 |
| P2 | 快照/分叉 | docker commit + 恢复 |

## 五、风险与取舍

- **卷清理**：删除沙箱必须删卷，否则磁盘只增不减；`DELETE` 路径与对账任务都要覆盖。
- **多 worker**：卷在创建它的 worker 上；沙箱恢复到**同一 worker**（`poder_id` 已固定），
  跨 worker 迁移需要卷迁移方案（暂不做，记录为已知限制）。
- **并发**：同一沙箱的 stop/start 需要串行化（状态机已有 RUNNING/STOPPING/STOPPED）。
