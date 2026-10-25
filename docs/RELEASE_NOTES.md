# SandrPod 0.6.0

> A sandbox that can be *trusted* to hold an agent, not just one that can run it.
> Three boundaries, each enforced where the previous one cannot see, plus the
> credentials an agent may use but never hold.

0.5.x ran code in an isolated filesystem and called it a sandbox. 0.6.0 adds the
parts that stand between that code and everything else on the machine: a
per-connection allow-list, an OS sandbox around every spawned process, a network
namespace when the platform has one, and sentinel credentials that only the proxy
can resolve. All of it is opt-in, all of it defaults off, and the one thing it
never does is quietly do nothing.

---

## 1. What is new

### 1.1 An egress allow-list, in three places

| Mode | Rules live | A tightened rule lands | Control plane down |
|---|---|---|---|
| `-egress-mode=enforce` | local file | on restart | fine |
| `-egress-mode=server` | both (polled copy) | ≤ `-egress-sync` (60s) | keeps the last copy |
| `-egress-mode=decider` | server only | next connection (≤10s cache) | **denies** |

The proxy is HTTP (absolute-form + CONNECT) and SOCKS5 (CONNECT only), bound to
loopback behind a per-process token. A name is resolved **once**, the address is
vetted (loopback, link-local, cloud metadata, this host's own addresses are
refused), and the vetted IP is dialled — no second lookup, so no rebinding
window. Every refusal carries a stable code (`X-Deny-Reason`) plus a sentence
written for the model. Container sandboxes get the same policy differently: a
per-pod `Internal: true` network plus an `egress-proxy` sidecar, so the container
has no route except the proxy.

### 1.2 An OS sandbox under the gate (`-os-sandbox`)

The permission gate covers the file API; the proxy covers proxy-aware clients.
Neither sees a process that calls `open(2)` or `connect(2)` directly — which is
what `Execute` and the PTY are. With `-os-sandbox=bwrap|auto|seatbelt` every
process the toolbox spawns runs inside bubblewrap or Seatbelt:

- **read-only root**, writable `work_dir` and temp dirs;
- **hardlocks masked** (`--tmpfs` / `--ro-bind /dev/null`), so `~/.ssh` is not
  merely off-limits to the file API, it is not there;
- **persistence paths read-only** — `.git/hooks`, `.git/config`, `.bashrc`,
  `.mcp.json`, IDE launch configs — found by a bounded walk and mounted over;
- **a network namespace** (Linux) with the proxy reachable through a relay on
  loopback: with egress enforcing, a process that ignores `HTTP_PROXY` reaches
  nothing at all;
- **capabilities dropped**, with `CAP_DAC_OVERRIDE`/`CAP_DAC_READ_SEARCH` kept
  back only when the agent runs as root and has to read a user's files.

`auto` degrades loudly when the backend cannot run; `bwrap`/`seatbelt` refuse to
start rather than run unprotected.

### 1.3 Credentials an agent can use but not read (`-credential-mask`)

Real values are replaced inside the sandbox by `fake_value_<uuid4>` sentinels and
substituted at the proxy, over TLS the proxy terminates:

- headers, and bodies up to 16 MiB (length-preserving, so `Content-Length` stays
  valid);
- credentials files (`~/.aws/credentials`, …) rewritten and bound over the
  originals;
- **AWS SigV4** re-signed with the real secret, checked against AWS's published
  example signature.

The cost is stated rather than hidden: for a terminated host the certificate is
ours, pinning breaks and mTLS cannot work. `-credential-terminate-exclude` keeps
such hosts as opaque tunnels, at the price of no masking for them, and a masked
credential over plain HTTP is refused (`masked_credential_cleartext`) rather than
leaked.

### 1.4 Two audiences for every refusal

`Reason` stays the localized string the person sees; `ModelReason` is one line of
English with what was refused and what to do instead, and it is what the tool
result hands back to the agent. Both go into the audit record — so "why did the
agent keep retrying" has an answer that is not a guess.

### 1.5 A fleet that says what it can enforce

A worker advertises its capabilities on the tunnel handshake, and the control
plane refuses to place a policy on one that cannot enforce it (`409`, "worker
capability unavailable"). The agent does the same in reverse: `server`/`decider`
modes probe `GET /api/v1/capabilities` and refuse to start against a control
plane that predates them, instead of running deny-all and looking like an outage.
The failure this prevents is silent — a sandbox reported `RUNNING` with no route
policy — which is why both are refusals rather than warnings.

### 1.6 The tooling follows

Everything above is reachable without curl: `sandrpod-cli egress get|set|clear`,
`create --egress-policy` / `--allow-domain` / `--deny-domain`; the Python,
LangChain and TypeScript clients all expose the same calls. The LangChain backend
was also brought into line with the sandbox protocol (`write` creates new files
only, `read(limit=0)` returns nothing, `upload_files` creates the parent), which
langchain's own conformance suite verifies: 69/86 pass, and every remaining
failure is one deepagents' own backend fails too.

---

## 2. Upgrading from 0.5.x

**Order matters once, and only once.**

1. **Upgrade the workers (poders) first.** A 0.5 worker ignores the `network`
   field in the create payload and starts the container unenforced. A 0.6 server
   will no longer let that happen silently: it answers `409 worker capability
   unavailable` and says which worker to upgrade. A sandbox *without* a policy
   still schedules onto an old worker, so a staged rollout is fine.
2. **Then the control plane.** The database migration is additive
   (`sandboxes.egress_policy`, `poders.capabilities`; both `ADD COLUMN … DEFAULT
   ''`), so an old binary can still read a database a new one migrated.
3. **Agents last, and only if you want the new modes.** An upgraded agent in
   `-egress-mode=server|decider` refuses to start against a 0.5 control plane,
   with a message saying so; `-egress-mode=enforce` needs nothing from the server.

Nothing changes for a deployment that upgrades without touching flags: every
feature is off by default.

### Breaking changes (Go API, for importers of `pkg/…`)

- `permission.AuditSink.Record` gained a `modelReason` parameter.
- `PoderRepository` gained `SelectBestFor(region, providerType, require)`.
  `SelectBest` remains, as a wrapper.
- `egress.Server.SOCKSURL()` returns `""` when the proxy listens on a unix
  socket, instead of a malformed URL.

The wire format, the HTTP API and the CLI are additive.

---

## 3. Known limits, stated

- **macOS Seatbelt is unverified.** The profile cannot be executed on a machine
  whose own shell is already sandboxed (an MDM-managed one), so it ships behind
  its probe and is documented as experimental. Linux is verified against real
  sandboxed processes.
- **The kernel floor's residuals**: write-protected paths are read-only
  *mounts*, so a hardlink to a protected file writes the same inode, and a
  command that creates `.git/hooks` and writes into it *in the same command*
  wins — the protection applies from the next spawn. Both need Landlock.
- **Terminated tunnels speak HTTP/1.1 only** (negotiating h2 would silently skip
  substitution), and masking does not cover what an upstream echoes back.
- **The macOS egress narrowing** blocks unix-socket DNS and inbound listeners by
  default; both are allowed explicitly, but the profile itself is unverified
  here.
- **Credential masking needs TLS termination**, and the CA key is as sensitive as
  the credentials it unmasks.

---

## 4. How this release was verified

| Layer | How |
|---|---|
| unit + race | `go test ./...` with `-race` on Linux (CI), plus 54 Python tests |
| real sandbox | `pkg/sandbox` tests in a privileged container: bwrap mounts, capability drop, PTY and process-group start shapes, netns + relay + terminating proxy |
| assembled product | `hack/playground/run.sh`, nightly: agent with all five layers, a container fleet walled through a CLI-created policy, and the mixed-version refusals against `-emulate-legacy` builds |
| third-party SDKs | the unmodified `e2b` 2.59.0 and `e2b-code-interpreter` 2.11.3, plus langchain's `SandboxIntegrationTests` |

Every one of those found at least one bug that the layer below could not see;
several are in the changelog with the failure they produced.

---

## 5. 中文摘要

0.5.x 只做了文件系统隔离。0.6.0 补上了代码与机器之间真正的那几层：出网白名单
（本地/轮询/逐连接三档，名字只解析一次、地址校验后拨号）、每个派生进程的 OS 沙箱
（只读根 + hardlock 遮蔽 + 持久化路径只读 + Linux 上的 network namespace + 保留
DAC 能力）、凭据 sentinel（沙箱里只有假值，代理在终止的 TLS 上替换，含 AWS SigV4
重签）、双受众拒绝文案，以及"worker 只接自己能强制的策略 / agent 拒绝不支持的
server"这一对混合版本关卡。

**升级顺序：先 poder，再 server，最后 agent。** 只有第一步是必须的：0.5 的 worker
会忽略策略字段，0.6 的 server 现在会以 409 拒绝而不是造出一个"看起来有墙"的沙箱。
数据库迁移是加列，来回滚动安全。所有新能力默认关闭，不动参数等于行为不变。

已知边界也写在 §3：macOS Seatbelt 未验证、只读挂载挡不住硬链接与"同一条命令内先建
目录再写"、终止隧道只谈 HTTP/1.1、掩码不管上游回显、CA 私钥等同于密钥。