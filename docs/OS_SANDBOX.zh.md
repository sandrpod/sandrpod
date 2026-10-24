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
- **持久化路径只读**：mandatory deny 那一组——`.git/hooks`、`.git/config`、
  `.bashrc`、`.mcp.json`、`.vscode`、`.claude/commands`……——保持可读、去掉写权限
  （Linux 用 `--ro-bind` 盖在可写树上，macOS 用末尾的 `deny file-write*`）。扫描深度
  由 `-mandatory-deny-depth`（默认 3）限定。

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

- **macOS 上网络是收窄，不是拿走。** 没有 network namespace 可拿，所以 profile 改为
  只允许出站到一个地方：代理的 loopback 端口
  `(allow network-outbound (remote ip "localhost:3128"))`。无视 `HTTP_PROXY` 的进程
  还能开 socket——只能开向代理，这正是目的；别处都到不了。这条只在探测通过时生效
  （见 §2），profile 本身在本机未经验证。
- **network namespace 只在 egress 生效时才有。** `--unshare-net` 却没有回到代理的
  路，会得到一个既不能解析也不能取任何东西的沙箱，所以两者是一起打开的（见 §4a）。
- **它不管 toolbox 自己的文件 API。** `fs/*` 是 toolbox 进程里的 Go 代码，不是派生
  命令；那是 permission gate 的边界，照旧生效。
- **它挡不住恶意的工具链设计。** profile 是策略，不是证明：漏一个遮蔽、或某条
  allow 比预期宽，就是洞。hardlock 用显式挂载遮蔽而不是靠规则"排除"，所以失败方式
  是可见的（一个路径要么被盖住，要么没有）。
- **macOS 的语义没被验证。** 见上表。

### 4a. 网络（Linux）

egress 代理生效时，沙箱同时拿到自己的 network namespace，代理改听 unix socket：

```
sandbox netns（只有 loopback）
   127.0.0.1:3128  ←  relay  ──→  host: /tmp/sandrpod-egress-*/proxy.sock
                                     （egress 代理，带策略）
```

- `--unshare-net` 让沙箱只剩 loopback——没有路由、没有 DNS、够不着宿主服务；
- 代理改听 **unix socket** 而不是端口（`egress.Options.UnixSocketPath`）；
- socket 目录和 agent 二进制（它自己兼作 relay：`sandrpod-agent relay …`）以只读
  方式 bind 进沙箱；
- wrapper 先起 relay、等就绪文件，再 `exec "$@"`——真正的 argv 走 `$@`，绝不做字符串
  拼接，因为那个参数可能是任意代码；
- 沙箱内的代理变量指向 `127.0.0.1:3128`，也就是 relay。

无视代理变量的进程现在什么都到不了。已在特权容器里端到端验证
（`pkg/sandbox/netns_linux_test.go`）：不走代理完全没路由；白名单主机经
relay → 代理取到；不在名单的主机经同一条路被拒。

为什么非要 relay：没有网络的沙箱够不着宿主上的 TCP 监听，而 `curl` 又不会说 unix
socket。relay 是桥接两者的最小东西，它自己没有任何策略——每个字节都交给代理，由代理
决定。

---

## 5. 自己验证

Linux，不用装任何东西（bubblewrap 不可用时测试自动 skip）：

```bash
GOOS=linux GOARCH=arm64 go test -c ./pkg/sandbox -o /tmp/sandbox.test
docker run --rm --privileged -v /tmp:/mnt alpine:3.20 \
  sh -c 'apk add -q bubblewrap && /mnt/sandbox.test -test.v -test.run Bwrap'
```

它对一个真实的沙箱化进程断言：允许的文件能读；hardlock 文件读不到（也不泄漏内容）；
hardlock 目录列不出来；工作树内可写；工作树外的写永远到不了宿主。
`-test.run NetnsRelay` 再补网络那半：不走代理完全没路由；白名单主机经 relay → 代理
取到；不在名单的主机被拒。

macOS 上，在一台本身没有被沙箱化的机器上：

```bash
sandrpod-agent -os-sandbox=seatbelt …     # profile 应用不了就拒绝启动
```

相关文档：[PERMISSION_AND_AUDIT.zh.md](PERMISSION_AND_AUDIT.zh.md)（它执行的那道门）、
[EGRESS_POLICY.zh.md](EGRESS_POLICY.zh.md)（网络那一半）、
[SANDBOX_RUNTIME_ANALYSIS.md](SANDBOX_RUNTIME_ANALYSIS.md)（模型来处）。