# SandrPod 0.7.1

> The release that made the cloud path real, and the one where a machine you attach
> stops being a machine you handed over.

0.7.0 turned the control plane into a library. This is what happened when the library
was driven by a commercial build against a real cloud account: five defects, each in a
different layer, each producing a symptom that pointed somewhere else. Between them,
**no provider other than `local` had ever worked end to end** — the paths existed,
ran, and failed late.

---

## 1. What the live Aliyun test found

The order below is the order they appeared. Each one hid the next.

| # | Symptom | Layer | What was wrong |
|---|---|---|---|
| 1 | `provider aliyun not found` | registration | providers were registered in `Main()`, which an embedding build never runs |
| 2 | VM created, then `docker: command not found` | bootstrap | Cloud Assistant's 60 s default killed the Docker install, and `Timeout` was read as success |
| 3 | worker started, never dialled | configuration | the VM was handed loopback as the control-plane address |
| 4 | first create timed out, container appeared afterwards | budget | the 550 MB sandbox image was pulled inside a two-minute create |
| 5 | **the VM outlived every sandbox** | reclamation | the idle reaper also only ran in `Main()`, and an orphan container counted as busy |

Only #3 and #4 were visible from the outside. #1, #2 and #5 needed the deployment to
be driven by something that did not go through `Main()` — which is exactly the
commercial build, and exactly why the open-source path had looked healthy.

The fixes are unremarkable individually. What they share is worth stating: **each
failure reported itself as a different problem than it was**, and none was reachable by
reading the code that owned the symptom.

Verified end to end afterwards on a real ECS instance: create → VM → Docker → worker
registered → sandbox `RUNNING` in 30 s → `execute` → delete → **idle reaper terminates
the VM**.

## 2. Device agents

Two changes, both about not surprising the person whose laptop it is.

**A workspace can attach one.** The route was `adminOnly`, so a hosted tenant could not
attach a machine at all. It accepts a worker credential now — but an ownerless record
is world-visible, so a worker credential without a tenant is refused, the record is
owned as `workspace:<tenant>`, and a name that belongs to someone else answers 409.

**It is confined by default.** The installer used to start it as root, from `/`, with
no gate and no kernel sandbox: everything, with no boundary to hit. Now it runs as the
invoking user, writes only under `~/sandrpod-work` (plus the system temp dir), gates
the rest, and uses the kernel sandbox where the platform has one. `--full-machine`
still opens all of it, and asks first.

Measured with the defaults on macOS: `$HOME`, `~/Documents`, `/usr/local/bin` and
`/private/var` refuse writes; `~/.ssh`, `~/Library/Mail`, `~/Library/Messages` and
`~/Library/Cookies` refuse reads; `/tmp` stays writable because build tools fail in
ways that look like bugs when it does not.

**Deleting one sticks.** It used to drop a pointer instead of the connection, so the
agent kept the socket, believed it was still serving, and its reconnect recreated the
record. Deleted names are tombstoned (refreshed on each rejected attempt, so a
pre-release agent cannot resurrect it either), the tunnel closes its transport, and an
agent told `410` stops and exits — with the installed service set to restart on
*failure* only, so a deliberate exit stays down.

## 3. Installers

`deploy/node-install.sh` (published at `docs.sandrpod.com/install.sh`) turns a fresh
machine into a worker: data disk first, then swap, then Docker from a mirror that
answers in China, then `daemon.json`, then the sysctls development tooling needs, then
the fleet. `deploy/agent-install.sh` (`/agent.sh`) turns a machine into a sandbox.

Both are idempotent, both accept `--flag=value`, both have `--dry-run`, and both were
written for the machine people actually run them on — which is why `agent.sh` on macOS
turned out to be the one that exited silently: `getent` does not exist there.

## 4. Upgrading

No Go API changed. Two behaviours a deployment may notice:

- `/api/v1/capabilities` grows a `providers` field. Old clients ignore it.
- A device agent deleted from the platform now **stops** instead of reconnecting. If
  you relied on "delete the record, the agent comes back", start the service again —
  that is one command now, not an automatic reconnect.

The image build no longer requires buildx: `FROM --platform=$BUILDPLATFORM` with the
variable unset is a parse error, and a plain `docker build` does not set it. Pass
`--build-arg BUILDPLATFORM=linux/arm64` on an arm64 host that has no buildx.

## 5. Known limits, unchanged

- **Placement is tenant-scoped, not tenant-isolated at the network layer.** Two tenants
  on one docker daemon share its bridge; run one daemon per tenant where that matters.
- **A provisioned VM is a worker, not a sandbox.** Deleting the sandbox leaves the
  worker for the next one, which is what `SANDRPOD_PODER_IDLE_TIMEOUT` exists for.
- **One region per provider per control plane.** The VSwitch and security group are
  region-scoped, so a request for another region fails with
  `InvalidVSwitchId.NotExist`.
- **A device agent has no isolation beyond the OS.** That is the point of it, and the
  reason it is not a shared fleet.

## 6. 中文摘要

0.7.1 是把云这条路走通的版本。用商业构建驱动一个真实的阿里云账号，暴露出五个缺陷，分处
不同层，而每一个的症状都指向别处：厂商没注册（注册写在 `Main()` 里，嵌入构建不执行）、
云助手 60 秒默认超时掐断 Docker 安装且超时被当成功、云主机被告知去连 `127.0.0.1`、
首次创建在 2 分钟预算内拉 550MB 镜像、以及**闲置回收同样只在 `Main()` 启动**，导致每台
云主机都在持续计费。

设备 agent 有两处变化：工作区现在可以自己接入一台机器（路由从 `adminOnly` 放宽到 worker
凭据，归属写成 `workspace:<租户>`，重名返回 409）；并且**默认就是关起来的**——以执行 sudo
的用户运行，只写 `~/sandrpod-work`（外加系统临时目录），其余交给权限门禁和内核沙箱，
`--full-machine` 才打开整台机器。

删除设备沙箱以前不生效：删的是指针不是连接，agent 保留 socket 继续以为自己在线，重连又把
记录建回来。现在删除会真正断开、记忆删除（每次被拒都刷新窗口），agent 收到 `410` 后退出，
服务配置改为"失败才重启"，所以主动退出不会被拉起来。

两个安装脚本：`/install.sh` 把一台空机器变成 worker，`/agent.sh` 把一台机器变成沙箱。都幂等、
都支持 `--flag=value`、都有 `--dry-run`。
