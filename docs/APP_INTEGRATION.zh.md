# 应用侧会感觉到什么

`docs/` 里其它文档讲的是机器怎么转。这一篇回答应用团队真正会问的问题：**升级之后我
会看到什么，我需要做什么？**

简短答案：SDK 和 HTTP API 不变，所有新层默认关闭。沙箱内有**三处**不同，有**四类**
工具会按设计坏掉——列在这里，免得半夜才发现。

## 一、调用侧：没有变化

没有新增必填参数，老调用照旧：

```python
sb = client.get_sandbox("my-sandbox")          # 不变
client.exec(sb, "python -c 'print(1)'")        # 不变
```

新能力都是可选的，且多数是给"跑平台的人"用的：

| 调用 | 谁用 |
|---|---|
| `POST /api/v1/sandboxes` 带 `network` | 平台：创建时就钉住策略 |
| `GET`/`PUT /api/v1/sandboxes/{name}/egress` | 平台：事后改 |
| `POST /api/v1/policy/decide` | agent 自己用（`-egress-mode=decider`） |

## 二、沙箱内：三处可见差异

### 2.1 环境变量会告诉你网络在哪

```bash
HTTP_PROXY=http://sandbox:<token>@127.0.0.1:3128
HTTPS_PROXY=http://sandbox:<token>@127.0.0.1:3128
SSL_CERT_FILE=/home/me/.sandrpod/egress-ca/trust-bundle.pem
NODE_EXTRA_CA_CERTS=…  REQUESTS_CA_BUNDLE=…  CURL_CA_BUNDLE=…  GIT_SSL_CAINFO=…
OPENAI_API_KEY=fake_value_4b1f…          # 开了掩码时
```

几乎所有客户端库都认这些变量，所以"能正常出网"这件事没变。要知道的一点：凭据变量里是
**sentinel**，不是密钥。比对它、哈希它、或者把它发给第三方的代码，看到的是假值——这正是
目的。

### 2.2 失败是有结构的，不再是超时

| 情况 | 应用看到 |
|---|---|
| 域名不在允许清单 | `403` + `X-Deny-Reason: not_allowed` |
| 被明确禁止的域名 | `403` + `denied_domain`（body 里是操作者写的原因） |
| 解析到 loopback/元数据地址 | `403` + `denied_address` |
| 决策服务连不上（`decider`） | `403` + `decider_unavailable` |
| 客户端无视代理（Linux netns） | `no route to host` / DNS 失败 |
| 掩码凭据走明文 HTTP | `403` + `masked_credential_cleartext` |

读 `X-Deny-Reason`，别读错误字符串：它稳定，而且能区分"策略说不行"和"网络坏了"——这正
是"换个办法"和"无限重试"的差别。

### 2.3 文件失败来自两个不同的层

- **permission gate**（toolbox 文件 API）：`403`，带一句给模型看的话，例如
  `/root/.ssh is hard-locked by the operator's policy and cannot be read or written; do not retry…`
- **内核**（bubblewrap/Seatbelt，进程自己 `open(2)`）：工具自己报的普通 `EACCES`/`EROFS`。

第二类是刻意的：把内核拒绝包装成平台错误，等于告诉模型"这是策略"，引导它去找绕过的方法。

## 三、会坏掉的工具

| 工具 | 为什么 | 怎么办 |
|---|---|---|
| 证书 pinning / mTLS 客户端 | 被终止的主机上证书是 SandrPod 签的 | 让操作者把它加进 `-credential-terminate-exclude`；该主机就没有掩码 |
| 必须 HTTP/2 的客户端 | 终止路径只谈 HTTP/1.1，否则替换会被协议升级静默跳过 | 允许降级，或把主机排除 |
| 硬编码直连的代码 | Linux 上沙箱除代理外没有路由 | 这是特性 |
| 往 `.git/hooks`、`.bashrc`、`.mcp.json`、IDE 启动配置写的安装器 | mandatory deny，gate 和内核都拦 | 改写到工作目录 |
| 读 `~/.ssh`、`~/.aws`、浏览器 profile | hardlock | 改用掩码凭据 |

## 四、应用代码的四条规矩

1. **用 `https://`**：掩码凭据只在被终止的 TLS 连接上替换，明文会被拒绝而不是泄漏；
2. **不要 pin 证书**：非要 pin，就把主机排除，并接受那台主机没有掩码；
3. **按 `X-Deny-Reason` 分支**：重试 `not_allowed` 是死循环，问人才是答案；
4. **写在 `work_dir` 里**：那是唯一可写树，也是持久化文件仍受保护的地方。

## 五、三种部署形态

### A. 本地开发——什么都不用做

所有层默认 `off`。`go run ./cmd/server` + `go run ./cmd/poder` 行为与以前一致。

### B. 容器舰队（poder）

全在控制面配，worker 不需要新 flag。

```bash
curl -X POST $API/api/v1/sandboxes -H 'Content-Type: application/json' -d '{
  "name": "build-1", "provider": "local",
  "network": {
    "allowed_domains": ["proxy.golang.org", "*.npmjs.org", "*.pypi.org"],
    "denied_domains": ["*.internal"],
    "denied_domain_reasons": {"*.internal": "internal services are not reachable from sandboxes"}
  }
}'
```

poder 会给每个 pod 一个 `internal` docker 网络加 `egress-proxy` sidecar，容器除代理外没有路由。

### C. 员工机（`sandrpod-agent`）

```bash
# 1. 只要出网白名单
sandrpod-agent -api-url=https://api.example.com -name=me-laptop -token=… \
  -egress-mode=enforce -egress-policy=~/.sandrpod/egress.json

# 2. 再加内核兜底（文件与网络都成为墙，Linux）
sandrpod-agent … -permission-mode=prompt -os-sandbox=auto

# 3. 全开：加凭据掩码
sandrpod-agent … \
  -permission-mode=prompt -os-sandbox=auto \
  -egress-mode=decider \
  -credential-mask=OPENAI_API_KEY,ANTHROPIC_API_KEY,GITHUB_TOKEN \
  -credential-mask-files=~/.aws/credentials \
  -credential-terminate-exclude=internal.corp.example
```

`-egress-mode` 是唯一值得想一下的选择：

| 值 | 规则在哪 | 收紧何时生效 | 控制面挂掉 |
|---|---|---|---|
| `enforce` | 本地文件 | 重启时 | 正常 |
| `server` | 两端（轮询副本） | ≤ `-egress-sync`（60s） | 用最后一份副本 |
| `decider` | 只在服务端 | 下一个连接（≤10s 缓存） | **全部拒绝** |

掩码用的凭据来自 **agent 自己的环境变量**——由启动它的人注入（launchd/systemd/你的平台），
不落配置文件。

## 六、十分钟验收

开了任何一层之后，这几条能证明它真的生效：

```bash
# 1. 启动日志逐层报告（唯一能证明"确实生效"的地方）
#    os sandbox: bwrap active — work_dir=… writable, 15 hard-locked path(s) masked, 2 persistence path(s) read-only
#    os sandbox: network namespace removed — the relay at 127.0.0.1:3128 is the only route out
#    egress: credential masking on for [OPENAI_API_KEY]; TLS is terminated for allowed hosts except []

# 2. 沙箱内
curl -s -o /dev/null -w '%{http_code}\n' https://example.com        # 200（在允许清单里）
curl -s -o /dev/null -w '%{http_code}\n' https://not-allowed.test   # 403
curl -s --noproxy '*' https://example.com                           # could not resolve host

# 3. 审计能回答"哪个 secret 去了哪"（只有名字）
jq -r 'select(.source=="egress") | "\(.host) \(.decision) terminated=\(.terminated) creds=\(.credentials)"' \
  ~/.sandrpod/audit/active.log | tail
```

每一层的手工完整走查（含预期输出）见 [PLAYGROUND.zh.md](PLAYGROUND.zh.md)。

相关：[CREDENTIALS.zh.md](CREDENTIALS.zh.md)、[EGRESS_POLICY.zh.md](EGRESS_POLICY.zh.md)、
[OS_SANDBOX.zh.md](OS_SANDBOX.zh.md)、[PERMISSION_AND_AUDIT.zh.md](PERMISSION_AND_AUDIT.zh.md)。
