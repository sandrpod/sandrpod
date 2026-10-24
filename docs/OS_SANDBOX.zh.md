# OS 沙箱（内核兜底）

> 可选。`-os-sandbox=off | auto | bwrap | seatbelt`，默认 `off`——和 permission
> gate 一样：会弄坏工具链的沙箱比没有更糟，所以这一层要显式打开。
>
> 英文版：[OS_SANDBOX.md](OS_SANDBOX.md)

在别人的电脑上跑 agent，有三层保护，各自管不同的事：

| 层 | 它看得到什么 | 它拦得住什么 |
|---|---|---|
| **permission gate**（`pkg/permission`） | 文件 API 调用、exec/PTY 启动 | agent 用**门内**接口去读 `~/.ssh` |
| **egress 代理**（`pkg/egress`） | 认代理的网络客户端 | `curl`/`npm`/`pip` 去连不在白名单的主机 |
| **OS 沙箱**（`pkg/sandbox`）——本文 | 子进程发出的每个系统调用 | 在 `Execute` 里 `open("~/.ssh/id_rsa")`；把代理变量一去再 `curl` |

前两层是**过滤器**：只看得见经过它们的东西。一行直接调 `open()` 或
`connect()` 的 Python 绕开两者——这就是"跑任意代码"的含义。这一层把进程装进平台
自己的沙箱，于是不管哪条代码路径发起，这些系统调用都会失败。

---

## 1. 它强制什么

两个平台同一套模型，这是有意的：

- **根只读**：磁盘可读（工具链在那儿），但哪里都不能写；
- **工作树可写**：`work_dir` 加上工具默认会用的临时目录；
- **hardlock 被遮蔽**：permission gate 锁住的每个路径——`~/.ssh`、`~/.aws`、
  `~/.sandrpod/permissions.json`、钥匙串……——对进程**不可读**，而不只是"文件 API
  不让读"。

Linux 上是 bubblewrap：

```
bwrap --ro-bind / / --bind <work_dir> <work_dir> --bind /tmp /tmp
      --ro-bind /dev/null <hardlock 文件> --tmpfs <hardlock 目录>
      --dev /dev --proc /proc --die-with-parent --unshare-pid --new-session -- <cmd>
```

macOS 上是生成的 Seatbelt profile，跑在 `sandbox-exec` 下，形状相同：
`(deny default)` → `(allow file-read*)` → 对 hardlock `(deny file-read* …)`
（SBPL 后写覆盖先写）→ `(allow file-write* …)` 给工作树。

toolbox 派生的每个进程都走它——`Execute`、session shell、PTY、
code-interpreter 内核、procmgr 启动——因为接口就放在 `pkg/toolbox/sandbox.go`，
和 `ChildEnv` 是同一类东西；漏掉接线的 spawn 点会被 `TestSandbox_*` 抓住。

---

## 2. 平台支持，照实说

| 平台 | 后端 | 状态 |
|---|---|---|
| Linux | `bwrap` | **已验证。** `pkg/sandbox/bwrap_linux_test.go` 真的执行一条被沙箱化的命令，并在特权容器里跑过。 |
| macOS | `sandbox-exec` | **实验性。** 写它的机器上没法跑：`sandbox-exec` 不能在已被沙箱化的进程里嵌套（`sandbox_apply: Operation not permitted`），而 MDM 管着的开发者 shell 正好是这种情况。靠下面的探测把关，allow 列表按真实负载加，别靠猜。 |
| Windows | — | 未实现。 |

`auto` 能用就用、不能用就说明为什么；`bwrap`/`seatbelt` 是**承诺**——起不来就不
启动 agent。一个"静默没生效"的 OS 沙箱，和一个"正在保护你"的沙箱从外面看一模一样，
而这正是这一层绝不能出现的情况。

探测不是纸面功夫。写这段代码的机器上：

```
os sandbox: auto requested but not usable on this host — processes run unwrapped
# 强行指定时：
os sandbox setup failed: sandbox-exec cannot apply a profile here (exit status 71):
    sandbox_apply: Operation not permitted
```

---

## 3. 配置

```bash
sandrpod-agent -api-url=https://api.example.com -name=my-laptop -token=… \
  -permission-mode=prompt \
  -os-sandbox=auto          # 或 bwrap / seatbelt 强制，off 关闭
```

| 参数 | 环境变量 | 默认 | 含义 |
|---|---|---|---|
| `-os-sandbox` | `SANDRPOD_OS_SANDBOX` | `off` | `off` \| `auto` \| `bwrap` \| `seatbelt` |

策略不单独配置：它直接读 permission store——`work_dir` 成为可写树，每一条
`scope: "hardlock"` 成为被遮蔽的路径。这是有意的。两份会各自漂移的"敏感路径"清单，
正是 hardlock 不再 hard 的原因；只有一份，这一层在内核能管的地方执行同一份。

启动时会把做了什么打出来：

```
os sandbox: bwrap active — work_dir=/home/alice/AgentSandbox writable, 17 hard-locked path(s) masked
os sandbox:   masked /home/alice/.ssh
os sandbox:   masked /home/alice/.sandrpod/permissions.json
…
```

---

## 4. 它不做什么

- **它不拿走网络。** Linux 上 `--unshare-net` 已实现（`Policy.NetworkNone`）但默认
  关闭：egress 代理听在宿主 loopback 上，没有桥接的沙箱连代理都够不着。补上它需要
  sandbox-runtime 那套中继（unix socket bind 进沙箱 + 里面一个极小的 relay），是
  另一件事。在那之前，笔记本上的出网仍是**过滤器**（见
  [EGRESS_POLICY.zh.md](EGRESS_POLICY.zh.md)）；容器里已经是墙。
- **它不管 toolbox 自己的文件 API。** `fs/*` 是 toolbox 进程里的 Go 代码，不是派生
  命令；那是 permission gate 的边界，照旧生效。
- **它挡不住恶意的工具链设计。** profile 是策略，不是证明：漏一个遮蔽、或某条
  allow 比预期宽，就是洞。hardlock 用显式挂载遮蔽而不是靠规则"排除"，所以失败方式
  是可见的（一个路径要么被盖住，要么没有）。
- **macOS 的语义没被验证。** 见上表。

---

## 5. 自己验证

Linux，不用装任何东西（bubblewrap 不可用时测试自动 skip）：

```bash
GOOS=linux GOARCH=arm64 go test -c ./pkg/sandbox -o /tmp/sandbox.test
docker run --rm --privileged -v /tmp:/mnt alpine:3.20 \
  sh -c 'apk add -q bubblewrap && /mnt/sandbox.test -test.v -test.run Bwrap'
```

它对一个真实的沙箱化进程断言：允许的文件能读；hardlock 文件读不到（也不泄漏内容）；
hardlock 目录列不出来；工作树内可写；工作树外的写永远到不了宿主；`--unshare-net`
真的拿掉了网络。

macOS 上，在一台本身没有被沙箱化的机器上：

```bash
sandrpod-agent -os-sandbox=seatbelt …     # profile 应用不了就拒绝启动
```

相关文档：[PERMISSION_AND_AUDIT.zh.md](PERMISSION_AND_AUDIT.zh.md)（它执行的那道门）、
[EGRESS_POLICY.zh.md](EGRESS_POLICY.zh.md)（网络那一半）、
[SANDBOX_RUNTIME_ANALYSIS.md](SANDBOX_RUNTIME_ANALYSIS.md)（模型来处）。