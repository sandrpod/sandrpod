# Anthropic Sandbox Runtime vs SandrPod

> 分析对象：https://github.com/anthropics/sandbox-runtime（ASRT，"srt"）
> 克隆版本：`v0.0.81`，`c66fb77`（Apache-2.0，TypeScript ~31k 行 + Rust `srt-win` + C seccomp + Java agent）
> 结论用于指导 SandrPod 的安全边界演进，不是 API 对照文档。
>
> **本文的两种来源会明确区分**：`[读码]` = 从源码/文档确认；`[实测]` = 本机实际跑通并贴了输出。

---

## 0. TL;DR

ASRT 和 SandrPod **不是同一层的东西，正因如此才互补**。

- ASRT 的原子是 **一个进程**：把任意命令塞进 OS 沙箱（macOS Seatbelt / Linux bubblewrap / Windows WFP），再把它唯一的出网通道收敛到宿主侧代理上。
- SandrPod 的原子是 **一台 VM / 一个容器 / 一个 agent 节点**：调度、生命周期、隧道、SDK 兼容、多租户。

ASRT 补的正好是我们自己在 [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) 里承认的两个洞：

1. **"It is not an OS-level sandbox"**（[:134-139](PERMISSION_AND_AUDIT.md#L134-L139)）——门只管住"经过门"的调用，`open("~/.ssh/id_rsa")` 直达 syscall。
2. **"Network egress. There is no allowlist yet … currently the largest gap"**（[:154-155](PERMISSION_AND_AUDIT.md#L154-L155)）。

**优先级建议**：先做 P0-1（出网策略），再 P1-5（工作区危险文件），再 P0-2（agent 模式的 OS 级强制）。**不要**把 ASRT 当唯一防线，它的信任模型（同用户、单机、代码基本可信）不适合我们的多租户远端执行。

> ✅ 顺带在本次分析中发现并**已修复**一个现存问题：`work_dir` 分支曾排在 hardlock 之前，员工从 home 目录启动 agent 时全部 hardlock（darwin 默认 17 条）失效（含 `permissions.json` 自提权）。现在 hardlock 先行，并在启动时告警。详见 [4.5](#45-顺带发现并已修复work_dir-分支曾排在-hardlock-之前)。

---

## 1. ASRT 是什么（按强制层拆）

```
srt <cmd>  →  OS 原语(seatbelt / bwrap / WFP)  +  进程内 HTTP+SOCKS 代理  +  违规事件采集
```

### 1.1 文件系统 `[读码]`

| 平台 | 机制 | 源码 |
|---|---|---|
| macOS | `sandbox-exec` + **动态生成 SBPL** | `src/sandbox/macos-sandbox-utils.ts:1045-1260` |
| Linux | `bubblewrap` bind mount（ro/rw/tmpfs） | `src/sandbox/linux-sandbox-utils.ts` |
| Windows (alpha) | 专用本地用户 `srt-sandbox` + WFP + 显式 ACE + write-restricted token | `src/sandbox/windows-sandbox-utils.ts`、`vendor/srt-win-src` |

Seatbelt profile 的第一行就是 `(deny default (with message "<logTag>"))`，之后**逐条开洞**：mach-lookup 白名单、`sysctl-read` 白名单、iokit 白名单、`ipc-posix-shm`、`pseudo-tty`（默认关）。

两个值得记住的坑：

- **规则后写覆盖先写**，所以读规则要分四层重排：`allow file-read*` → `deny` → `allowWithinDeny` → 再补回被 allow 冲掉的 deny；写规则还要在最后补 `file-write-unlink` / `file-write-create`，否则 `mv` 能把被保护的文件挪走（`macos-sandbox-utils.ts:861-960`）。
- Linux 侧 bwrap 自己的限制：argv 32 页（128 KiB），超了就把 mount 列表写进 `O_TMPFILE` 匿名文件，用 `--args 9` 让 bwrap 读 fd；bwrap 最多 9000 个参数（约 3000 个 mount），超了在 wrap 期就报 `LinuxSandboxProfileError`（带 `.code` 便于分支）。

### 1.2 网络 `[读码]`

- 两个代理：**HTTP**（绝对 URI + CONNECT）和 **SOCKS5**，另有一个 **mux 前端**把两者合到一个端口（`src/sandbox/mux-proxy.ts`）。
- **Linux**：`bwrap --unshare-net` 直接删掉网络命名空间，只把代理的 unix socket bind 进去，用 socat 桥接。`[读码: linux-sandbox-utils.ts:1491-1510]`
- **macOS**：Seatbelt 只放行到 localhost 代理端口的连接。
- **Windows**：WFP `ALE_AUTH_CONNECT` 按 SID 拦截，只放行到代理端口段。
- 环境变量注入是"兼容层"而不是边界：`HTTP_PROXY/HTTPS_PROXY/ALL_PROXY`，外加 `GIT_SSH_COMMAND`（git over ssh）、`JAVA_TOOL_OPTIONS`（JVM javaagent）、`NODE_USE_ENV_PROXY`（Node 22.21+）。
- **解析地址校验**（`resolved-address-guard.ts`）：白名单按**名字**匹配，但在 dial 之前解析一次，丢掉 loopback / link-local / multicast / 云 metadata（`100.100.100.200`、`168.63.129.16`、`fd00:ec2::/32`…）/ 本机所有网卡地址 / `deniedResolvedAddresses` 里的段，并且**用通过校验的那个地址去 dial**（不做第二次解析）。这是防 SSRF 的关键，也是"允许 `*.example.com`"不会变成内网跳板的原因。

### 1.3 系统调用 `[读码]`

Linux 用 `apply-seccomp`（x64/arm64 静态二进制）：外层 bwrap 建沙箱 → socat 桥先起来 → `apply-seccomp` 再建**嵌套的 user+PID+mount namespace**（PID 1 设 `PR_SET_DUMPABLE=0`）→ fork 后 `prctl()` 装 BPF → exec 用户命令。BPF 拦 `socket(AF_UNIX, …)`（默认禁 unix socket）以及 `io_uring_setup/enter/register`（否则 `IORING_OP_SOCKET` 绕过 socket 规则）。bwrap 侧统一 `--cap-drop ALL`。

### 1.4 策略模型 `[读码]`

| 维度 | 语义 | 备注 |
|---|---|---|
| 读 | `denyRead` + `allowRead`，**allow 优先**（deny-then-allow） | 但更具体的 deny 仍赢：`allowRead:["."]` + `denyRead:["**/.env"]` 仍然拒 |
| 写 | `allowWrite` + `denyWrite`，**deny 优先**（allow-only） | 读写优先级故意相反 |
| 网络 | 默认全禁，只 `allowedDomains`（支持 `*.x.com`、`:443` 端口后缀、IPv6 括号字面量） | `deniedDomains` 先判；`strictAllowlist` 可禁掉"未命中就问" |
| 凭据 | `credentials.envVars/files`，`mode` 取 `deny` / `mask` | mask 需 TLS 终止；文件掩码只在 Linux 生效，macOS/Windows 降级为 deny |

### 1.5 违规与"回喂模型" `[读码]`

这是我认为 ASRT 最被低估的部分：

- 三个来源统一进 `SandboxViolationStore`（100 条 ring）：macOS 读系统 sandbox 日志、Linux 走 seccomp USER_NOTIF 旁路 socket（`linux-violation-monitor.ts`）、代理拒绝。
- 每条违规按 `commandId` 归因（键只取前 100 字符；未注册的键会被 sanitize 后截断——因为**沙箱内进程可以写这些载体**）。
- `annotateStderrWithSandboxFailures(commandId, stderr)` 把"为什么被拦"追加到 stderr 再交给模型。
- deny 可以带 **给模型看的原因**（`deniedDomainReasons`、ask callback 的 `{allow:false, reason}`），它会出现在违规行、`<sandbox_violations>` 块、以及 CONNECT 的 403 status phrase 里。

### 1.6 外部决策器协议 `[读码]`

`srt proxy` 只跑代理、不跑子进程，每个请求交给独立 **decider** 进程判定（`docs/srt-proxy.md`）：

- 帧 = 4 字节大端长度 + 一个 UTF-8 JSON；`hello` / `req` / `verdict`(allow|deny|need_body) / `body`。
- 严格解析：非 UTF-8、重复键、`null`、多余字段、空的可选字段都算协议违规。
- **一个 5 s 超时就 fail-closed**；decider 挂了 → 挂起请求 503、代理退出；永不"没有 decider 也转发"。
- 拒绝码固定：`host_not_allowed` / `decider` / `decider_unavailable` / `bad_request` / `misdirected` / `opaque_tunnel` / `plaintext_header_set` / `bodyless_method_body` / `address_not_allowed` / `too_many_tunnels` / `method_refused`。
- 限额表（隧道数、握手超时、缓冲上限）直接写进文档。

### 1.7 配置语义 `[读码]`

- 没有 `~/.srt-settings.json` → 用内置默认；**文件在但非法 → 报错退出 1**，绝不回落到默认（因为默认是"另一个配置"，丢掉的是 denyRead/credentials）。
- `--control-fd <fd>`：逐行读完整配置并**整份替换**；README 明确列出"哪些改动热生效（网络列表、sigv4、mitm 相关）、哪些不（文件规则）"。
- `checkDependencies()` 的契约很干净：**errors = 跑不起来，warnings = 功能降级**。

### 1.8 工程纪律 `[读码]`

- `test/sandbox/` 下 118 个测试文件，用 fast-check 做属性测试（glob、路径穿越、winsrt 路径）。
- CI 6 组 OS/arch（含 Windows ARM），每条安全不变量基本对应一条测试。

---

## 2. SandrPod 现状（自查）

| 层 | 现状 | 位置 |
|---|---|---|
| 准入 | 五分支：hardlock → work_dir → permanent → session → ask；弹窗 fail-close | [pkg/permission](../pkg/permission/manager.go) |
| 覆盖范围 | 文件 API、`/process`、`/procmgr/start`、PTY；`/code-interpreter` **只审计不拦截** | [policy.go](../pkg/permission/policy.go#L1-L38) 自述 |
| 命令策略 | token 扫描，明确承认可被 `base64 -d`、`"s"+"cp"` 绕过 | 同上 |
| 文件边界 | 黑名单前缀匹配 + `EvalSymlinks` | [executor.go](../pkg/toolbox/executor.go#L216) |
| 容器 | 只设 network mode + CPU/内存；无 cap-drop / no-new-privileges / seccomp / 只读 rootfs；镜像无 `USER` | [docker.go](../pkg/poder/docker.go#L135-L143)、`docker/Dockerfile.toolbox` |
| 网络出口 | 无任何策略 | [sandbox_store.go](../pkg/sandpod/sandbox_store.go#L43-L63) 无 network 字段 |
| 审计 | NDJSON，只记**决策**，不记"被底层拒绝的操作" | [event.go](../pkg/audit/event.go#L46) |
| 内核级强制 | **零**：`grep -r "sandbox-exec|bwrap|seccomp|landlock" pkg/ cmd/ --include=*.go` 无命中 | — |

---

## 3. 差异矩阵

| 维度 | ASRT | SandrPod |
|---|---|---|
| 隔离原语 | 内核（seatbelt/bwrap/WFP/seccomp） | 容器/VM（poder）+ **agent 模式纯进程** |
| 信任模型 | 同用户、单机、代码基本可信 | 多租户、远端、代码不可信 |
| 策略载体 | 本机 JSON | 控制面 API（+ 本机 `permissions.json`） |
| 决策点 | 每连接 ask / 每请求 decider | 每调用 5 分支 + 弹窗 |
| 网络 | 默认全禁 + 强制代理 + 地址校验 + 可选 MITM | **完全放行** |
| 凭据 | sentinel 掩码 + 按主机注入 + sigv4 重签 | 无概念（agent 直接看见 env） |
| 写保护 | 自动 mandatory deny + 深度可配扫描 + 父目录 pin | 静态 hardlock 列表（手工 seed） |
| 违规 | 结构化事件 + 回喂模型 + 可忽略规则 | 只有决策日志 |
| 生命周期/多租户 | 无 | 强项：调度、TTL、快照、8 云、E2B 兼容 |
| 跨平台一致性 | 三套语义，文档里大量差异说明 | 一套 Go 语义 |

---

## 4. 实测记录 `[实测]`

环境：macOS 27.0.1 / arm64，Node v22.23.3。**没有 Docker daemon**，因此 Linux 的 bwrap 路径未能实跑。
ASRT 从源码构建：`npm install --ignore-scripts` + `tsc`（`husky` 的 prepare 在 CI 外会失败，用 `--ignore-scripts` 绕开）。

### 4.1 Seatbelt 在本机不可用（重要发现）

```
$ /usr/bin/sandbox-exec -p '(version 1)(allow default)' /bin/echo hello
sandbox-exec: sandbox_apply: Operation not permitted      # exit 71
```

连最宽松的 profile 都装不上——因为**当前进程本身已经处在一个 seatbelt 沙箱里**（本 harness），Seatbelt 不能嵌套。这有两层含义：

1. 我们的测试机/CI 若跑在 MDM/EDR 或任何已沙箱化的上下文里，`srt` 的 macOS 路径直接不可用。
2. SandrPod 的 **agent 模式**如果也要套 seatbelt，必须显式检测并给出可读的降级提示，而不是静默失败——ASRT 自己也是这么处理的（`checkDependencies` 区分 errors/warnings，Linux 另有 `enableWeakerNestedSandbox`）。

> 结论：**P0-2 的平台适配成本必须计入"宿主环境已被沙箱化"这一情形**，这条在文档里几乎没人写。

### 4.2 网络策略（不依赖 seatbelt，可单独验证）

`SandboxManager.initialize()` 只启动宿主代理，不需要真正 wrap 命令，因此可以直接对着代理发请求。代理要求 **Basic 认证**：用户名 `srt`（或 `srt.<encodedCommand>` 做 per-command 归因），密码 = `getProxyAuthToken()`。

实测结果（allowlist = `localhost:8123`）：

| # | 请求 | 结果 |
|---|---|---|
| A | `http://localhost:8123/` | `200 TARGET-OK` |
| B | `http://127.0.0.1:8123/`（IP 字面量，未列入） | `403 blocked-by-allowlist` |
| C | `http://example.org/` | `403 blocked-by-allowlist` |
| D | `http://evil.test/`（deniedDomains） | `403 blocked-by-allowlist` |
| E | `CONNECT localhost:8123` | `200 Connection Established`（未配 MITM 时隧道透传） |

注意 B：**显式把 loopback 写进白名单**（A）才放行，按 IP 访问未列入的本地端口被拒——这正是地址校验在起作用。

### 4.3 违规事件与"给模型的原因"

```js
SandboxManager.initialize(cfg, async ({host, port}) =>
  ({ allow: false, reason: `${host}:${port} is not approved; use the internal mirror at mirror.corp:443` }))
```

```
undecided -> ask callback denies with reason: 403 Connection blocked by network allowlist
--- violation store ---
[deny] deny network-outbound not-listed.example:80 (not-listed.example:80 is not approved; use the internal mirror at mirror.corp:443)
```

客户端拿到的是通用 403 文案，**违规行里带着可用于指导模型的 reason**——这个"两个受众、两套文案"的设计值得直接抄。

### 4.4 凭据掩码（端到端跑通）

配置 `network.tlsTerminate: {}` + `credentials.envVars:[{name:'CORP_API_KEY', mode:'mask'}]`，手动 `register('CORP_API_KEY', 'sk-live-REALVALUE-1234567890', ['postman-echo.com'])`，然后以 `Authorization: Bearer <sentinel>` 通过 CONNECT + MITM 访问 `postman-echo.com/get`：

```
CA minted: true | sentinel: fake_value_99ddf658-3b91-4488-b364-8e00910d079b
upstream saw : Bearer sk-live-REALVALUE-1234567890
real arrived : YES
sentinel leak: no
```

同一个 sentinel 发往**另一个**已白名单但不在 `injectHosts` 的主机：

```
A postman-echo.com (injectHosts)   -> REAL VALUE injected
B httpbin.org      (not in list)   -> sentinel kept (no laundering)
```

即"凭据 A 的 sentinel 不能借凭据 B 的白名单主机洗白"。这条 gating 是 ASRT 设计里最容易做错、也最容易漏测的一环。

> 实现细节（读码补充）：sentinel 形如 `fake_value_<uuid4>`，并**按真实值长度随机补齐**，让 `Content-Length` 在 body 替换时保持不变（否则要退回 chunked framing）；真值只存在进程内存，不落盘、不写日志。

### 4.5 顺带发现并已修复：`work_dir` 分支曾排在 hardlock **之前**

读 [manager.go](../pkg/permission/manager.go) 时发现的、和 ASRT 正好相反的一处顺序问题：

```go
// 修复前
// 1. work_dir is the silent-allow zone.          ← 先返回
if m.workDir != "" && pathInside(req.Path, m.workDir) { return allow }
// 2. Hardlock check — must run before any allow-rule lookup ...
if hit := matchHardlock(...) { return deny }
```

而 agent 的 `work_dir` 默认是**启动目录**（容器内为 `/workspace`，否则 `os.Getwd()`，见 `pkg/toolbox/platform_unix.go:48-57`），`-work-dir` 不传就不 chdir。也就是说：**员工从自己的 home 目录启动 `sandrpod-agent`（双击、launchd、`~/` 下执行都很常见），`work_dir` 就是 `$HOME`，`~/.ssh`、`~/.aws`、乃至 `~/.sandrpod/permissions.json` 全部落在第一分支里被静默放行，全部 hardlock（darwin 默认 17 条）形同虚设**——最后一条还意味着 agent 可以改写自己的策略文件完成自提权。

ASRT 在这点上的不变量是明确的：**mandatory deny 对 `allowWrite` 根目录内部同样生效**（"even with `allowWrite: ["."]`, writing to `.bashrc` … will fail"）。

**已按 ASRT 的不变量修复**（本次改动）：

1. `checkBody` 里 hardlock 判定提到 `work_dir` 之前——hardlock 是沙箱的不变量，`work_dir` 只是便利，不能反过来放宽它；
2. 新增 `permission.HardlocksUnder(dir, rules, home)` + agent 启动告警：`work_dir` 吞掉 hardlock 时打印 `WARNING`，并提示用 `-work-dir` 收窄（保留宽 `work_dir` 的可用性，只是不再静默）；
3. 回归测试：`TestCheck_HardlockBeatsWorkDir`、`TestCheck_HardlockBeatsWorkDir_PolicyFileNotWritable`（自提权）、`TestCheck_WorkDirStillSilentAroundHardlocks`（宽度仍在）、`TestHardlocksUnder`。

顺带：`permissions.json` 本来就是 0600（`pkg/permission/store.go:216`），所以问题不在文件权限，而在于**它落进了静默放行区**。

---

## 5. 可借鉴清单

每条给出：**做什么 / 为什么 / 落点 / 验收 / 成本**。

### P0-1 出网 allowlist + 强制代理

- **做什么**：per-sandbox 网络策略 + 宿主侧 HTTP/SOCKS 代理 + 解析地址校验 + 结构化拒绝原因。
- **为什么**：唯一"不做就等于没做安全"的项；今天一个被 prompt-injection 的 agent 一条 `curl` 就能把整台机器读到的数据送走。
- **落点**：
  - `pkg/egress/`：`proxy.go`（absolute-form + CONNECT）、`socks.go`、`policy.go`（匹配 + `:port` + 通配）、`resolve.go`（地址校验，直接照抄 ASRT 的 deny 集合：loopback/link-local/multicast/云 metadata/本机网卡）、`auth.go`（每沙箱一次性 token）。
  - API：`CreateSandboxRequest` 加 `network{allowedDomains, deniedDomains, deniedDomainReasons, allowLocalBinding}`，随 job 下发。
  - Agent 模式：设 `HTTP_PROXY/HTTPS_PROXY/ALL_PROXY` + `NODE_USE_ENV_PROXY` + `GIT_SSH_COMMAND`；能上内核强制时配 netns/seatbelt，不能时就靠环境变量（**此时是"礼貌"不是"边界"**，必须在 UI/日志里说清楚）。
  - 容器模式：`--network none` + 代理 sidecar，或 docker 网络只留代理可达。
- **验收**：沙箱内 `curl http://example.org` → 403 且带 reason；`curl http://169.254.169.254/` → 拒；白名单域名按 IP 访问 → 拒；非代理感知工具（`ssh`/`git`）也被覆盖。
- **成本**：2–3 周（Go 版代理约 1.5–2k 行）。
- **状态：用户态 + 控制面已实现**。HTTP（absolute-form + CONNECT）与 SOCKS5
  （仅 CONNECT）双代理、per-process token、`:port`/`*.`/IPv6 括号条目、解析地址
  校验（loopback / link-local / multicast / 云 metadata / 本机网卡）、拒绝原因
  回传（`X-Deny-Reason` + body）、`source: "egress"` 审计事件、
  `toolbox.SetChildEnv` 把代理注入全部 spawn 点。
  控制面侧：策略进 `CreateSandboxRequest.network`、建沙箱时校验、落
  `sandboxes.egress_policy`、`GET/PUT /api/v1/sandboxes/{name}/egress` 读写；
  agent `-egress-mode=server` 以 deny-all 起步、轮询热替换（`Server.UpdatePolicy`）。
  容器侧：`pkg/poder/egress.go` 用每 pod `Internal: true` 网络 + `cmd/egress-proxy`
sidecar，容器除代理外无路可走（已在本机 Docker 实测）。
尚未做：员工机的内核兜底（netns / Seatbelt）、TLS 拦截与凭据掩码。

### P1-2 把 hardlock 下沉成内核事实（仅 agent 模式）— 🟡 已实现（`pkg/sandbox`）

`-os-sandbox=auto|bwrap|seatbelt`：toolbox 派生进程全部套进 bwrap（Linux）/ Seatbelt
（macOS）——根只读、work_dir 可写、hardlock 被遮蔽（`--tmpfs` / `--ro-bind /dev/null`，
SBPL 则是后置 deny）。策略直接读 permission store，避免两份清单漂移。
**Linux 已实测**（特权容器里跑真实沙箱化命令：hardlock 读不到、目录列不出、树外写
落不了盘、`--unshare-net` 真的断网）；**macOS 未验证**——`sandbox-exec` 不能在已沙箱化
的进程里嵌套，只能靠启动探测把关，文档里标了 experimental。
**Linux 上网络也拿走了**：`--unshare-net` + 代理改听 unix socket + 沙箱内 relay
（`sandrpod-agent relay`）——无视代理变量的进程什么都到不了，已在特权容器里端到端
验证。仍未做：macOS 的网络收窄（Seatbelt 可只放行 localhost:代理端口，但本机无法
验证）。

- **做什么**：由 `permissions.json` 生成强制层——macOS 生成 SBPL 跑 `sandbox-exec`；Linux 用 bwrap 把 hardlock 路径 tmpfs/ro-bind 覆盖掉。
- **为什么**：`open("~/.ssh/id_rsa")` 目前直达 syscall，hardlock 对"跑代码"无效；这是把"consent gate"升级成"沙箱"的唯一途径。
- **落点**：`pkg/sandboxctl/`（backend 抽象：`seatbelt` / `bwrap` / `none`），agent flag `-sandbox-backend`；profile 生成复用 permission 的规则模型（读写 + hardlock + mandatory deny 一起编译）。
- **坑（ASRT 踩过）**：读写优先级相反；`file-write-unlink`/`file-write-create` 必须在最后补；Linux 上不存在的 deny 路径要先造 mount point（会短暂出现在宿主上，`git config` 会因此报 lock 错误）；argv 长度上限。
- **验收**：`python3 -c "print(open('$HOME/.ssh/id_rsa').read())"` → `PermissionError`；`echo x >> .git/hooks/pre-commit` → `EPERM`。
- **成本**：4–6 周（两平台），必须单独立项。

### P1-3 decider 协议 → 中心化策略

- **做什么**：把 `pkg/permission` 变成可远程调用的 decider，控制面持有 org 级 allowlist。
- **为什么**：ASRT 已经把协议、超时、fail-closed、拒绝码定死了，直接对齐能省一轮设计；多租户产品本来就不该把策略放本机 JSON。
- **落点**：`pkg/policydecider/`（帧编解码 + 校验）+ 控制面 `POST /api/v1/policy/decide`；字段沿用 `req/verdict`，拒绝码沿用 `X-Deny-Reason`。
- **验收**：decider 超时 5 s → 全部 503 且不转发；decider 崩溃 → 代理退出；非法帧 → 链路断开。
- **成本**：1–2 周。

### P1-4 凭据掩码

- **做什么**：agent 只拿到 `fake_value_<uuid4>`，真值由代理在出网时按主机注入。
- **为什么**：employee-PC 场景里 agent 一定需要公司 key（模型 API、内部服务），而现在它明文可见、可写进日志、可被 prompt injection 带出。
- **落点**：`pkg/credentials/`（sentinel registry + injectHosts gating + body 替换）、`pkg/egress` 的 header/body hook、审计事件 `credential.inject`（只记 name + host，不记值）。
- **前置**：需要 TLS 终止（MITM CA），建议**按沙箱 opt-in**，别默认开；mTLS 上游和 pinning 客户端必须走 `excludeDomains` 例外。
- **验收**：4.4 的两个用例（注入 + 不洗白）做成集成测试。
- **成本**：2 周（不含 CA 分发运维）。

### P1-5 工作区危险文件自动防护 — ✅ 已实现

- **做什么**：把 ASRT 的 mandatory deny 变成 permission 的**第六分支**，且**排在 work_dir 与 permanent 授权之上**：`.git/hooks`、`.git/config`、`.bashrc/.zshrc/.profile`、`.gitconfig`、`.mcp.json`、`.vscode/`、`.claude/commands/`、`.idea/`；扫描深度默认 3（1–10 可配）。顺带修掉 4.5 的分支顺序。
- **为什么**：现在是固定 hardlock 列表，而真正的持久化后门在**工作目录里**——agent 往 `.git/hooks/pre-commit` 写一行就长期驻留，且下次 `git commit` 在沙箱外执行；而 4.5 说明连现有 hardlock 在默认配置下都可能被 work_dir 短路。
- **落点**：`pkg/permission/mandatory.go` + 在 `Manager.Check` 分支链最前面插入；Linux 侧用 `rg` 扫描到深度 N。
- **验收**：`allowWrite: ["."]` 下写 `.git/hooks/pre-commit` 被拒；`git commit` 正常；`.mcp.json` 不可改。
- **注意**：4.5 的分支顺序修复已把「危险路径不受 work_dir 放行」这条不变量立住了，本项要把 mandatory deny 接到同一个位置（hardlock 之前）。
- **成本**：3 天。

### P1-6 违规事件 + 双受众文案

- **做什么**：① 把"被拒的操作"也记进审计（今天只记决策）；② `Reason` 拆成 `UserReason`（中文弹窗）与 `ModelReason`（英文、一句话、带替代方案）。
- **为什么**：ASRT 证明了"告诉模型为什么被拒 + 该怎么办"能显著减少重试循环；我们现在 `CheckPTY` 的 `Reason` 是中文 UI 文案（`manager.go`），会一路进审计和工具返回值。
- **落点**：`pkg/audit/event.go` 加 `kind: "net_deny" | "fs_deny"`、`host`、`rule`；`permission.Decision` 加 `ModelReason`；MCP/tool result 里优先用 ModelReason。
- **验收**：模型收到 `blocked: example.org:443 — use mirror.corp:443`，且审计里两条文案都在。
- **成本**：3 天（含 P0-1 的接入）。

### P2-7 配置语义与信任边界

- 策略文件**非法必须拒绝启动**，不能回落默认。
- **项目内配置永远不能提权**（ASRT 对 `allowAppleEvents` 专门警告"绝不能来自被检出的仓库"）。对应我们：`permissions.json` 本身已是 0600，但见 4.5——只要它落在 `work_dir` 里，agent 就能通过文件 API 改写它完成自提权。
- 热更新照 `--control-fd` 的写法：整份替换、逐条校验、文档明说"哪些即时生效、哪些要重启"。

### P2-8 能力自检与"降级可见"

`checkDependencies()` 的 errors/warnings 二分法 + 每个"弱化开关"单独命名并附安全警告。对应我们：agent 启动时把能力矩阵（seatbelt/bwrap/代理/审计上传）上报控制面并在 tray 显示；`-permission-mode=off` 这类降级必须在 UI 上显式可见，而不只是日志。

### P2-9 测试与文档范式

- 为 `resolveSafePath` 补属性测试（symlink、`~`、`/tmp → /private/tmp`、前缀 `..`、大小写不敏感 FS）——这是 fast-check 最划算的地方。
- 文档补三样东西：**限额表**、**安全模型**、**已知限制**。ASRT 的 README 有专门章节，我们目前只有散落的注释。

---

## 6. 不建议照搬

1. **进程级信任模型**：ASRT 假设同用户、单机、代码基本可信。我们跑不可信代码，主流路径必须留在容器/VM，srt 只能当纵深防御的一层。
2. **把 Seatbelt 语义当跨平台标准**：ASRT 自己为此写了大量"Linux 上这个 glob 会被跳过""macOS 上这条规则优先"的说明。保持"平台中立策略模型 + 每后端编译器"。
3. **默认开 TLS MITM**：CA 分发、mTLS、pinning 都会碎；做成一等公民但按沙箱 opt-in。
4. **Windows WFP + 专用本地用户**：复杂度极高（ASRT 自己标 alpha 且列了一堆不覆盖场景）。Windows 先做代理，别一步到位。
5. **把网络策略放本地 JSON**：我们是控制面产品，策略必须经 API 下发、进审计、可多租户。

---

## 7. 路线图与风险

| 阶段 | 内容 | 工期 | 关键风险 |
|---|---|---|---|
| 0 | ~~修 4.5 的分支顺序（hardlock 先于 work_dir）~~ ✅ 已完成：含启动告警与 4 个回归测试 | 0.5 天 | 已确认 `work_dir` 的静默放行不变 |
| 1 | P0-1 出网代理 + 地址校验 + 拒绝原因 — ✅ 用户态、控制面下发、容器强制均已完成；剩员工机内核兜底 | 2–3 周 | 非代理感知工具绕过（Linux 需 netns，macOS 需 seatbelt）；DNS rebinding |
| 2 | P1-5 mandatory deny + P1-6 违规事件/双文案 | 1 周 | 误伤（`git` 需要 `.git/config` 可读不可写，要精确到 hooks/config） |
| 3 | P1-2 agent 模式 OS 级强制 | 4–6 周 | 宿主已沙箱化（见 4.1）、两平台语义差异、argv/mount 上限 |
| 4 | P1-3 decider 中心化 + P1-4 凭据掩码 | 3 周 | CA 分发与合规；性能（每请求一次 RTT） |

**贯穿性风险**：一旦我们宣称"沙箱"，就必须能回答"哪些路径没被覆盖"。建议每个阶段都更新 [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) 的 "What this stops, and what it does not" 一节——那份诚实清单是项目目前最值钱的资产之一。

---

## 8. 附录：复现步骤

```bash
# 1. 构建 ASRT（CI 外要跳 husky）
git clone --depth 50 https://github.com/anthropics/sandbox-runtime.git
cd sandbox-runtime && npm install --cache /tmp/npm-cache --ignore-scripts && ./node_modules/.bin/tsc

# 2. 网络策略（不需要 seatbelt）
node <script>   # initialize() → getProxyPort() / getProxyAuthToken()
#   代理认证：Basic base64("srt:<token>")   ← 用户名必须是 srt，否则 407

# 3. 凭据掩码（需要 credentials 块，否则 injector 不接线）
#   network.tlsTerminate: {} + credentials.envVars:[{name, mode:'mask'}]
#   getSentinelRegistry().register(name, realValue, injectHosts)
```

本次实验脚本留存于 `/tmp/srt-lab/`（`net-demo2.mjs`、`net-demo3.mjs`、`cred-demo.mjs`、`laundering-demo.mjs`），未入库。