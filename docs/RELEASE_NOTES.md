# SandrPod release notes

The running, per-commit list is [CHANGELOG.md](../CHANGELOG.md); the public
narrative version is [docs.sandrpod.com/release-notes](https://docs.sandrpod.com/release-notes).

## 0.13.7 (current)

Three security fixes that came out of reading the worker boundary and the file
gate against what they were assumed to protect.

**File searches now authorize every path they read, not just their root.** A
search of an authorized directory authorized that directory once, then read
everything beneath it: searching `~` returned the contents of hard-locked
`~/.ssh` files, a symlink inside the root was followed anywhere, and a `../`
glob climbed out. Each reached path is now resolved and dropped when it leaves
the root, hits the read blacklist, or lies under a hardlock, and a search reads
regular files only (a FIFO could previously hang it).

**Revoking the last API key can no longer leave the server open.** An empty DB
key index used to mean "no credentials configured", so revoking the last key
turned a running server into an anonymous-admin endpoint; the index now latches
once it has held a key, and the last key is refused unless a static token
exists. The refusal is atomic with the delete, so two concurrent revocations
cannot both win.

**`GET /api/v1/jobs/poll` is gone.** Nothing polled — creates are pushed down
the worker's tunnel and re-delivered by the resumer — but any worker credential,
including a tenant's provision token, could call it and receive every tenant's
pending create requests with their environment variables.

## 0.13.6

Five AWS findings that only showed up when the provider was read against the
scheduler and then exercised on a live account.

A terminated instance now reports `TERMINATED` instead of `STOPPED` (a caller
waiting for removal, or an orphan sweep, saw a machine that never went away);
`GetVM` reports the region it was asked for, not the availability zone, in that
field; `Cleanup` refuses to run without `SANDRPOD_AWS_ALLOW_CLEANUP=1`, because
it terminates every SandrPod instance in the region regardless of workspace;
`ListInstanceTypes` asks `DescribeInstanceTypes` (1 373 types in `us-east-1`,
paginated) with the old static list kept as an offline fallback; and the EBS
volume is tagged alongside the instance, so cost allocation and tag-based orphan
searches see it.

## 0.13.5

The cloud providers, audited against what the scheduler assumes and then run on
real accounts.

**AWS** launches with IMDSv2 required and a hop limit of 1 (sandboxes are
containers on that host, and the metadata service holds the instance role's
credentials), carries a per-call `ClientToken` so an SDK retry cannot become a
second billing instance, refuses a public IP without a subnet and a region that
differs from `AWS_REGION`, and honours `SANDRPOD_SPOT_AWS`. Only `Success` counts
as a successful SSM command now; deleting an already-terminated instance is not
an error; a create whose machine is not running when the waiter returns says so
instead of handing back a pending VM.

**Tencent** is the same treatment plus what only a live mainland run found: TAT
kills a command after 60 seconds by default and the provider never set `Timeout`
— every command now carries 15 minutes, and one that never reached the instance
(`DELIVER_FAILED` / `START_FAILED`) is retried. Spot (`SPOTPAID`) requires a max
price, so `SANDRPOD_SPOT_MAX_PRICE[_TENCENT]` is plumbed through and the provider
names the setting when it is missing. A delete that races an in-flight
termination retries instead of reporting a fabricated "left running".

On a mainland network, pin `SANDRPOD_TOOLBOX_IMAGE` / `SANDRPOD_EGRESS_IMAGE`:
the scheduler pre-pulls them while it provisions, which is the difference
between a slow create and one that times out on its first image pull.

## 0.13.4

A security narrowing and two operational repairs. A **worker credential** can no
longer use the sandbox API — it exists for the tunnel handshake, the job poll, its
own heartbeat, the enrolment exchange and the two calls an agent makes directly
(capabilities, egress), and everything else is refused in the middleware from an
explicit list. An **async create is `PROVISIONING`, not `PENDING`, while a VM
boots**, so the resume pass no longer mistakes a multi-minute provisioning for a
lost delivery (that exact race failed a real provisioning three seconds before its
worker registered); a row left behind by a crash is reclaimed after 30 minutes.
**Aliyun spot** is available behind `SANDRPOD_SPOT_ALIYUN` — off by default, with
on-demand unchanged. `poder -version` reports the build, and
`deploy/prometheus/sandrpod-alerts.yml` turns `/metrics` into five alerts.

## 0.13.3

Exports `server.VerifyProvisionToken` so an embedding build that supplies its own
`Config.Authenticator` can accept the credential the scheduler mints for a
provisioned VM. That front door never runs the built-in token scheme, so without
the exported check it rejected every machine the scheduler booted. No behaviour
change for a server using the built-in authenticator.

## 0.13.2

Two fixes that matter more than their size. Provisioned VMs no longer receive
the platform token: the scheduler mints a per-VM credential (worker role, scoped
to the requesting tenant, signed with the platform secret) so a machine it booted
cannot administer the fleet, and the SSH/run-command hop that delivers it leaks
at worst that one worker's credential. And the outbox claim now compares the
timestamp a row actually carries: a job written by anything other than this
process (an import, an operator's `psql`, an older build) spells the same instant
differently, which made its claim match zero rows and left the delivery PENDING
forever with nothing in the log.

The multi-instance check runs its resumer on a 2 s tick
(`SANDRPOD_RESUME_STALE_AFTER` / `SANDRPOD_RESUME_INTERVAL`) so it tests the
logic instead of racing the production cadence. No schema change; workers do not
need to move to use either fix, but upgrading them keeps versions aligned.

## 0.13.1

Fixes the pinned-create path used by the console: it now sets
`persist_data` by default, so a desktop sandbox keeps `/workspace` across
stop/start. `sandrpod-cli` stays at 0.7.3.

## 0.13.0


The first release with an **opt-in gVisor runtime**. `runtime: "gvisor"` on a
create (CLI `--runtime gvisor`) runs the sandbox under `runsc` on a worker whose
Docker daemon registers it; workers advertise the `gvisor` capability, and a
request without a capable worker is refused with `409` instead of silently
running under `runc`. The node installer installs and registers gVisor by
default (`--no-gvisor` opts out).

gVisor sandboxes get an explicit upstream resolver (`SANDRPOD_GVISOR_DNS`), an
`/etc/hosts` mapping for the egress sidecar, and live egress updates. Snapshots
and fork are refused (`409`): `docker commit` does not capture runsc writes.
GPU + gVisor is refused (`400`). See [GVISOR.md](GVISOR.md).

The Python CLI is `sandrpod-cli 0.7.3`, adding `--runtime`.

## 0.12.1

Fixes for the E2B snapshot and fork paths added in 0.12.0:

- a fork is created on the worker that holds the source snapshot, and a pinned
  create follows the same tenant rule as normal placement;
- `create_snapshot` returns the familiar `repo:tag` Docker actually stores, so
  `list_snapshots` and `delete_snapshot` agree with it.

Upgrade order is unchanged: workers first, then the control plane, then agents.
Nothing in this release changes the native HTTP API or the schema.

## 0.12.0

The E2B surface became complete enough to run the official SDK without caveats,
and same-workspace sandboxes stopped sharing a network:

- E2B `create_snapshot` / `list_snapshots` / `delete_snapshot`, `fork`, and
  `PUT /sandboxes/{id}/network`;
- `allowPublicTraffic=false` maps to per-sandbox private ports, and `cpuCount`
  is honoured and reported back;
- every non-egress sandbox gets its own bridge (`sandrpod-s-<hash>`);
- CPU/memory/storage defaults are explicit (1 vCPU / 1 GiB / 2 GiB), and
  ownerless legacy records are admin-only;
- the egress sidecar image is pulled on demand.

The Python CLI is versioned independently; `sandrpod-cli 0.7.2` shipped with the
matching client fixes.

## Historical: 0.7.1

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
