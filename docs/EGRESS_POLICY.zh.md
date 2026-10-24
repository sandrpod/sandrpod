# 出网策略（Egress policy）

> 对"沙箱里的进程能连到哪"的可选过滤器。默认关闭（`-egress-mode=off`），和
> permission gate 是同一种姿态：容器/云 VM 里的沙箱，网络边界由底座提供；真正
> 需要这一层的，是 **sandrpod-agent 本身就是那台机器** 的部署。
>
> 英文版：[EGRESS_POLICY.md](EGRESS_POLICY.md)

[PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) 里列过 consent gate 不管什么，
"网络出口"原本排第一：一个能跑 `curl` 的 agent 就能把读到的东西送出去。这份文档
是补上这个缺口的那一层——以及同样重要的，它补不上的那一部分。

---

## 1. 它处在哪一层

| 层 | 作用 | 本版本 |
|---|---|---|
| **出网策略**（`pkg/egress`） | 决定沙箱进程能到哪些目的地，其余拒绝并给出模型能照做的原因 | ✅ 本文档 |
| **解析地址校验**（`pkg/egress/guard.go`） | 允许的是**名字**，但名字解析到 loopback / 内网 / 云 metadata / 本机地址时拒绝 | ✅ 本文档 |
| **内核兜底**（network namespace、Seatbelt、bwrap、WFP） | 拦住不认代理变量的进程 | ❌ 不在本版本，见[路线图](#7-它不做什么) |

最后一行是必须说清的部分。代理是**过滤器**：它覆盖所有认
`HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` 的工具——而这正是 agent 日常在跑的
东西（curl、wget、npm、pip、cargo、go、走 http 的 git、各种 API SDK、包管理器）。
它不覆盖直接开 socket、无视环境变量的进程。请把它理解成"agent 没法随手把数据
带出去"，而不是"这台机器气隙隔离"。

---

## 2. 快速开始

```bash
# 1. 写策略。空的 allow 列表 = 完全禁止出网，所以每条策略都从"这次任务真正需要
#    什么"开始写。
cat > ~/.sandrpod/egress.json <<'JSON'
{
  "allowed_domains": ["api.github.com", "*.npmjs.org", "registry.npmjs.org:443"],
  "denied_domains": ["*.internal.corp"],
  "denied_domain_reasons": {
    "*.internal.corp": "internal hosts are not reachable from this sandbox; ask the operator for a mirror"
  },
  "denied_resolved_addresses": ["10.0.0.0/8", "192.168.0.0/16"]
}
JSON

# 2. 带着它启动 agent。
sandrpod-agent -api-url=https://api.example.com -name=my-laptop \
  -permission-mode=prompt \
  -egress-mode=enforce -egress-policy=~/.sandrpod/egress.json
```

agent 会先把将要执行的策略打到日志，再把代理地址交给每个子进程：

```
egress: enforcing from /Users/me/.sandrpod/egress.json (http=127.0.0.1:52311 socks=127.0.0.1:52312)
egress: allow=[*.npmjs.org api.github.com registry.npmjs.org:443]
egress: deny=[*.internal.corp]
```

沙箱里看到的效果：

```console
$ curl https://api.github.com/zen
Keep it logically awesome.

$ curl https://example.org/
example.org:443 is not on the egress allow list
curl: (56) ... # HTTP 403, X-Deny-Reason: not_allowed
```

`-egress-mode=enforce` 但没给 `-egress-policy` 是启动错误。空 allow 列表会拒绝
一切，而从外部看，"策略为空"和"网络坏了"长得一模一样——不如直接拒绝启动并说明
原因。

---

## 3. 策略文件

JSON，snake_case，**未知字段直接报错**（拼错一个键会得到一个空 allow 列表，然后
静默拒绝一切）。

| 字段 | 含义 |
|---|---|
| `allowed_domains` | 允许到达的目的地。空 = 完全禁止出网。 |
| `denied_domains` | **先判定**，无条件拒绝。`*` 表示全拒。 |
| `denied_domain_reasons` | 为 `denied_domains` 里的条目（精确匹配）指定模型看到的原因，替代通用文案。 |
| `denied_resolved_addresses` | 被允许的**名字**不得解析到的额外 CIDR。 |
| `strict_allowlist` | 显式声明"未命中即拒绝"。本来就是这行为；这个开关存在，是为了将来加上"逐次询问"时它不会被悄悄放宽。 |

### 条目语法

| 条目 | 匹配 |
|---|---|
| `example.com` | 精确匹配 `example.com`，任意端口 |
| `*.example.com` | `a.example.com`、`a.b.example.com`——**不含** `example.com` 本身 |
| `api.example.com:8443` | 该主机，且仅该端口 |
| `127.0.0.1:3000` | IPv4 字面量，锁定端口 |
| `[::1]:8080` | IPv6 用方括号，RFC 3986 风格 |
| `*` | 一切（放 `denied_domains` 里有用，放 `allowed_domains` 里是自伤） |

`*` 只支持作为开头的 `*.` 标签；`api.*.example.com` 会被拒绝。条目是
host[:port]——写成 URL、带路径或以 `/` 结尾都是配置错误，在加载时就报出来。

优先级是重点，和 [sandbox-runtime](https://github.com/anthropics/sandbox-runtime)
一致：

1. `denied_domains` 命中 → **拒绝**，再宽的 allow 也救不回来；
2. `allowed_domains` 命中 → 允许连接，**但仍须通过下面的地址校验**；
3. 都没命中 → 拒绝，原因里写清楚该往策略里加什么。

---

## 4. 解析地址校验

allow 列表匹配的是名字。谁控制了一个被允许名字的 DNS——或者被允许通配符下的任意
标签——就控制了它解析到哪。没有第二道校验的话，允许 `*.example.com` 就等于允许
访问 `127.0.0.1`、内网和 `169.254.169.254`，只等哪天有人把某个子域指过去。

所以，在真的 dial 一个被允许的**主机名**之前，代理只解析一次，丢掉所有落在禁止类
里的地址，连到幸存的那个。**通过校验的那个地址就是被 dial 的地址**，不做第二次
解析（第二次解析既是 TOCTOU 窗口，也是 DNS rebinding 的入口）。

默认永远拒绝：

- loopback（`127.0.0.0/8`、`::1`）、unspecified（`0.0.0.0/8`、`::`）
- link-local（`169.254.0.0/16`、`fe80::/10`）、multicast（`224.0.0.0/4`、`ff00::/8`）
- 不在 link-local 里的云 metadata 端点：`100.100.100.200`、`168.63.129.16`、
  `192.0.0.192`、`fd00:ec2::254`、`fd20:ce::254`、`fd00:c1::a9fe:a9fe`、`fd00:42::42`
- **本机任一网卡上的地址**——绑在 `0.0.0.0` 的服务，从 LAN 地址访问和从 loopback
  访问是同一个服务

默认**不**拒绝：私有地址段（`10/8`、`172.16/12`、`192.168/16`、`fc00::/7`）。允许
一个内网主机是正当需求，所以这交给运营者决定；不需要时把网段写进
`denied_resolved_addresses`。

**例外**：`allowed_domains` 里的 IP 字面量是明确的决定——`127.0.0.1:3000` 就表示
那台机器那个端口，类检查不会去质疑它。同一个地址经由**名字**访问是另一回事，仍然
拒绝，这正是 guard 存在的意义。

名字解析不到任何地址时，报告的是**连接失败**而不是策略拒绝：模型需要能区分
"DNS 坏了"和"策略说不"，因为两者的修法不同。

---

## 5. 沙箱看到什么

toolbox 派生的每个进程——一次性 `Execute`、常驻 session shell、PTY、
code-interpreter 内核、procmgr 启动——都继承：

```
HTTP_PROXY, http_proxy, HTTPS_PROXY, https_proxy = http://sandbox:<token>@127.0.0.1:<port>
ALL_PROXY, all_proxy                             = socks5://sandbox:<token>@127.0.0.1:<port>
NODE_USE_ENV_PROXY=1
```

`NO_PROXY` 故意**不设**：它是绕过名单，能往里写主机的沙箱就能绕开策略。

两个监听都只绑 `127.0.0.1`，并要求一个每进程生成的 token（192 位，启动时生成，
绝不落盘）。同一台机器上的两个 agent 不能互借 allow 列表，同用户的其它本地进程
也不能。

拒绝长这样：

```
HTTP/1.1 403 Forbidden
X-Deny-Reason: not_allowed
Content-Type: text/plain; charset=utf-8

example.org:443 is not on the egress allow list
```

| `X-Deny-Reason` | 状态码 | 含义 |
|---|---|---|
| `not_allowed` | 403 | 没有任何 allow 条目覆盖该目的地 |
| `denied_domain` | 403 | 命中 `denied_domains`（body 里是 `denied_domain_reasons` 的文案） |
| `denied_address` | 403 | 名字被允许，解析出的地址不允许（loopback / metadata / 本机 / 列出的 CIDR） |
| `proxy_auth_required` | 407 | 没带 token 或 token 不对 |
| `bad_request` | 400 | 不是 absolute-form 请求 / 请求目标无法解析 |
| `upstream_unreachable` | 502 | 策略放行了，网络没通 |

SOCKS5 用 `0x02`（connection not allowed by ruleset）回应拒绝。只实现 `CONNECT`：
`BIND` 与 `UDP ASSOCIATE` 直接拒绝而不是半支持——UDP 中继需要自己的一套 allow
语义，`BIND` 则会让沙箱接受入站连接，与出网控制恰好相反。

有一个 SOCKS5 的细节值得先说清楚：**自己解析域名的客户端**（`curl --proxy
socks5://…`，以及大多数 SOCKS 库的默认行为）会把 **IP 字面量**发过来，而 allow
列表匹配的是**名字**——这种请求除非该字面量本身被列出，否则会被拒绝。请改用远端
解析：curl 用 `socks5h://`，PySocks 用 `rdns = true`，这样代理才能看到它能判断的
主机名。

---

## 6. 审计

每次拒绝都会成为一条 `source: "egress"` 的审计事件；设置
`Options.LogAllowed` 后允许也会记录（agent 默认不记——生产规模下 allow 流是噪声）。

```json
{
  "event_id": "…", "occurred_at": "2026-10-24T12:00:00Z",
  "source": "egress", "decision": "deny", "caller": "connect",
  "host": "example.org", "port": 443,
  "code": "not_allowed", "rule": "",
  "reason": "example.org:443 is not on the egress allow list"
}
```

`address` 只在 `denied_address` 时出现。模型看到的是**类别**（"resolves to a
loopback address"），审计日志留下具体地址。这个拆分是有意的：地址对排查问题的
运营者有用，对设法绕过过滤的模型也有用。

拒绝事件与 permission 决策写进同一个 NDJSON 日志，走同一个上传器
（`-audit-upload-url`），一个 cursor 覆盖两者。

---

## 7. 它不做什么

列出来，是因为一个夸大自己的安全边界比没有边界更糟：

- **无视代理变量的进程照样能连。** 内核兜底（容器的 network namespace、
  员工机的 Seatbelt/bwrap）是下一块工作，也是把"过滤器"变成"墙"的东西。
- **不做 TLS 拦截。** CONNECT 隧道是不透明的：只过滤目的地，不看内容。这也是
  凭据掩码为什么不在这一版——往解密后的请求里注入密钥需要每沙箱一个 CA，
  以及它的一整套运维故事（mTLS 上游、证书 pinning、信任包），是独立项目。
- **没有逐连接的"问人"。** 未命中即拒绝。每次连接弹一次窗，是个带限流问题的
  UX 难题。
- **没有 UDP，没有入站。** 只有 SOCKS5 CONNECT。
- **一个 agent 一个代理。** 这台机器上所有沙箱共享同一份策略。按沙箱下发策略
  需要控制面把它放进创建 job 的 payload，那是容器模式的后续工作。
- **策略在启动时读取。** 改文件要重启 agent 才生效；热更新必须先说清楚哪些改动
  对已在隧道上的连接无效（答案是：全部无效）。
- **不覆盖 git over SSH。** `ssh` 不读任何代理由变量。修法是 sandbox-runtime 那套
  `GIT_SSH_COMMAND` + `socat`，代价是每台员工机都得装 `socat`；这一版没做。

---

## 8. 怎么验证它有效

```bash
# 允许：走沙箱拿到的代理
curl -x "$HTTP_PROXY" https://api.github.com/zen

# 策略拒绝
curl -x "$HTTP_PROXY" https://example.org/
# → 403，body：example.org:443 is not on the egress allow list

# 地址校验拒绝（先把 "localhost:8080" 加进 allowed_domains）
curl -x "$HTTP_PROXY" http://localhost:8080/
# → 403，X-Deny-Reason: denied_address，body：… resolves to a loopback address

# token 是"别的本地进程"和"你的 allow 列表"之间唯一的东西
```

对应的单测与端到端测试在 `pkg/egress/`：`policy_test.go`（条目语法与优先级）、
`guard_test.go`（地址类别，注入 resolver）、`proxy_test.go`（真实 HTTP 客户端、
真实 CONNECT 隧道、真实 SOCKS5 握手，以及各种鉴权拒绝）。

---

## 9. 配置参考

| 参数 | 环境变量 | 默认 | 含义 |
|---|---|---|---|
| `-egress-mode` | `SANDRPOD_EGRESS_MODE` | `off` | `off` \| `enforce` |
| `-egress-policy` | `SANDRPOD_EGRESS_POLICY` | — | 策略 JSON 路径，`enforce` 时必填 |

相关文档：[PERMISSION_AND_AUDIT.zh.md](PERMISSION_AND_AUDIT.zh.md)（与它互补的
同意门）、[SANDBOX_RUNTIME_ANALYSIS.md](SANDBOX_RUNTIME_ANALYSIS.md)（模型的来处）。