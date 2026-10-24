# 凭据：agent 能用，但拷不走

> 可选。`-credential-mask=OPENAI_API_KEY,GITHUB_TOKEN`，默认关，且要求 egress 生效。
> 打开后，**允许清单内的主机其 TLS 会被终止**——这句话有代价，先读 §2 再读 §1。

需要付费 API 的 agent 就得有钥匙。钥匙放在它的环境变量里，它就能读、能打印、能发到
egress 策略允许的任何地方——而"允许的任何地方"比"本来就该去的地方"宽得多。这个特性让
钥匙**可用而不可读**：

```
沙箱里：  OPENAI_API_KEY=fake_value_4b1f…        （sentinel）
   │
   ├─ 请求头带 "Authorization: Bearer fake_value_4b1f…"
   ▼
egress 代理（终止 TLS，在出口处替换）
   │
   └─ 请求头变成 "Authorization: Bearer sk-real-…"   → 上游
```

真实字节只存在于 agent 的内存和代理发出的请求里。它们不在沙箱的环境变量、文件、命令行
或审计日志中。

## 一、掩码覆盖什么

| 面 | 做法 | 位置 |
|---|---|---|
| 环境变量 | 沙箱拿到 sentinel 而不是真值 | `Masker.Env()`，覆盖继承来的环境 |
| 请求头 | 每个头的值都扫描；sentinel 被替换 | `Masker.substituteHeaders` |
| 请求体 | 上限 16 MiB，**等长替换**让 `Content-Length` 仍有效 | `serveTerminated` |
| 文件（`~/.aws/credentials`、`~/.netrc`…） | 重写后的副本只读 bind 盖在原文件上 | `-credential-mask-files`、`Policy.ReadSubstitute` |
| AWS SigV4 | 用真 secret 重算签名 | `Masker.ResignAWS` |

sentinel 是 `fake_value_<uuid4>`；真值更长时补到等长。等长很关键：替换后字节数不变，
代理就不用重新分帧，而重新分帧正是 chunked 与 length 之争的来源。

SigV4 特殊，因为它是唯一不能"原地换值"的凭据：签名是对整个请求的 HMAC，用 sentinel 签
过的请求必须用真 secret 重签。触发点是 `Credential=` 作用域里出现 sentinel。实现是拿
AWS 官方示例签名做校验的（`pkg/egress/sigv4_test.go`）——重签号签错一点点，报错和"密钥
错了"一模一样，这种 bug 能藏几个月。

Linux 上文件掩码是把假文件 bind 盖在真路径上，真字节**根本不在沙箱命名空间里**。macOS
没有可替换的挂载点，所以退化为**拒绝读**：工具坏掉，而不是读到真值。体验更差，失败更
正确。

## 二、代价，明说

TLS 终止意味着沙箱的连接不再终结于上游。对每个被终止的主机：

- 沙箱看到的证书由 **SandrPod 的 CA** 签发，不是上游签的。证书 pinning 失效；到上游的
  mTLS 不可能工作（客户端证书只在原客户端手里，而现在客户端是我们）；
- 代理看见明文，包括它刚替换进去的真值。代理、以及磁盘上的 CA 私钥，都成为这条流量
  的信任锚，敏感程度不亚于凭据本身；
- 沙箱里任何"我端到端验证了对端"的推理都不再成立。

有两个逃生口，因为代价是真的：

- **`-credential-terminate-exclude=<domain>`** 保持不透明隧道：不终止、不替换，正常
  端到端 TLS。mTLS 和 pinning 的站点用它。这些主机也就**没有掩码**——这是交换，不是
  疏漏。
- **`-credential-allow-plaintext`** 允许在明文 HTTP 上替换。默认关，因为那会把真值放到
  线路上；不开时，带 sentinel 的明文请求会被拒（`X-Deny-Reason:
  masked_credential_cleartext`），既不放行也不泄漏。

需要说清到底哪条信任关系变了。沙箱从没信任过宿主——宿主能读同一份文件系统、替换二进制、
观察进程。这个特性改变的是**沙箱与上游**之间的关系：它被有意切断，并转移给了代理。
需要这条关系完整的工作负载，放进排除列表。

## 三、配置

```bash
sandrpod-agent -api-url=https://api.example.com -name=my-laptop -token=… \
  -egress-mode=enforce -egress-policy=~/.sandrpod/egress.json \
  -credential-mask=OPENAI_API_KEY,ANTHROPIC_API_KEY,GITHUB_TOKEN \
  -credential-mask-files=~/.aws/credentials,~/.netrc \
  -credential-terminate-exclude=internal.corp.example     # mTLS 上游
```

| 参数 | 环境变量 | 含义 |
|---|---|---|
| `-credential-mask` | `SANDRPOD_CREDENTIAL_MASK` | 要掩码的凭据名，值从 agent 自己的环境读 |
| `-credential-mask-files` | `SANDRPOD_CREDENTIAL_MASK_FILES` | 被重写并覆盖的文件（支持 `~`） |
| `-credential-ca-dir` | `SANDRPOD_CREDENTIAL_CA_DIR` | CA 存放位置（默认 `~/.sandrpod/egress-ca`） |
| `-credential-terminate-exclude` | `SANDRPOD_CREDENTIAL_TERMINATE_EXCLUDE` | 永不终止的域名 |
| `-credential-allow-plaintext` | `SANDRPOD_CREDENTIAL_ALLOW_PLAINTEXT` | 允许在 HTTP 上替换 |

CA 首次运行生成并**持久化**：每次重启都换 CA 会逼沙箱里所有工具重新信任，并弄坏缓存
了证书的东西。`ca.key` 权限 `0600`，按密钥对待。

agent 通过环境变量把 CA 交给沙箱里的运行时（`SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS`、
`REQUESTS_CA_BUNDLE`、`CURL_CA_BUNDLE`、`GIT_SSL_CAINFO`、`AWS_CA_BUNDLE`、`DENO_CERT`、
`PIP_CERT`），都指向同一个 bundle 文件，内容是我们的 CA **加上**系统根——这样工具仍然
能验证被排除的、或根本没走代理的主机。

Linux 上 bundle 含系统根。macOS 的系统库是 keychain 视图，不一定能枚举；那时 bundle 只有
我们的 CA，终止就覆盖所有允许的 443 主机，被排除的主机需要自带根证书。启动日志会写明是
哪种情况。

掩码时 `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` 会自动链接，
这是重签的前提。

## 四、审计

被终止的连接会被标记，凭据按**名字**记录：

```json
{"source":"egress","kind":"connect","host":"api.openai.com","port":443,
 "decision":"allow","terminated":true,"credentials":["OPENAI_API_KEY"]}
```

值从不出现。这条记录有两处值得留：审计者能看出哪段对话把哪个凭据发到了哪里；`terminated`
标记把"端到端 TLS 被打破"变成一个可见事件，而不是脚注。

## 五、怎么验证

单元与集成测试在任何平台都能跑：

```bash
go test ./pkg/egress -run 'Masking|ResignAWS|MaskFile|CA_'
```

覆盖：sentinel 形状与等长、会被混淆的值（拒绝）、对真实 TLS 上游做头与体的替换、上游读到
真值而客户端只有 sentinel、被排除主机确实是不透明隧道（客户端握手失败，因为我们不是
上游）、明文 sentinel 被拒、SigV4 重签到 AWS 官方签名、CA 跨重启持久化。

端到端那条跑的是一个真实沙箱化进程，在真实 network namespace 里，特权容器中：

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./pkg/sandbox -o /tmp/sandbox.test
docker run --rm --privileged -v /tmp:/mnt alpine:3.20 \
  sh -c 'apk add -q bubblewrap && /mnt/sandbox.test -test.v -test.run MaskedCredential'
```

它一次断言三件事：进程自己没有路由、请求经 relay 到达代理、上游读到真凭据——而沙箱自己
的输出里从不包含它。

## 六、它不做什么

- **它挡不住刻意的读取。** 代理有真值，agent 有真值；能读 agent 内存或 CA 私钥的人也有。
  它防的是**模型**——那个会写你的命令的东西。
- **它不管上游把东西回显回来。** 一个行为糟糕的服务把 key 放进响应体，沙箱就拿到了。
  这里不看响应。
- **它不覆盖终止隧道内的 h2。** 终止路径只协商 HTTP/1.1，是刻意的：协商 h2 会让每个用它
  的请求静默跳过替换。
- **它不发现凭据。** 你点名掩码什么。
- **它不让 macOS 的文件掩码工作。** macOS 上被掩码的文件是拒绝读，不是替换——工具坏掉，
  而不是泄漏。

相关：[EGRESS_POLICY.zh.md](EGRESS_POLICY.zh.md)（它依赖的允许清单）、
[OS_SANDBOX.zh.md](OS_SANDBOX.zh.md)（下面的内核兜底）、
[PERMISSION_AND_AUDIT.zh.md](PERMISSION_AND_AUDIT.zh.md)（门与审计）。
