# SandrPod 0.7.0

> The release that makes SandrPod **embeddable**: a control plane another program
> can import, three seams it can plug into, and tenant isolation that holds on
> every path — including the ones nobody was looking at.

0.6.0 finished the boundaries around a sandbox. 0.7.0 turns the API server into a
library: `pkg/server` replaces `cmd/server`'s guts, a host build supplies its own
authentication, admission and event sink, and E2B compatibility becomes a handler
you mount rather than a binary you run. Everything the commercial build needs, and
none of it required: `cmd/server` behaves exactly as before.

---

## 1. What is new

### 1.1 `pkg/server`: the control plane as a library

`server.New(cfg, stores, tunnelStore, directStore)` returns the mux that
`server.Main()` serves. The flags move into a private `FlagSet` inside `Main()`,
so an embedding program can define its own `-port`/`-db` without a
`flag redefined` panic — a bug that only appears when the second program starts.

`NewE2BGateway(domain, cfg, stores, tunnelStore, directStore)` exposes the
E2B-compatible surface as a handler. Mount it at your own path, or not at all.

### 1.2 Three seams a different build can plug into

| Seam | Decides | Built-in |
|---|---|---|
| `Config.Authenticator` | who a request is | the token scheme |
| `Config.Admission` | whether a create may proceed | nothing |
| `Config.Events` | what happened | nothing |

`Admission` is called early *and* at the create choke point, so it must be
idempotent; a refusal at the choke point now marks the job and the record failed
instead of leaving a sandbox `PENDING` forever. `Events` payloads carry owner,
`cpu_cores`, `memory_mb`, `ttl_seconds` and `poder_id`, so a metering sink does
not have to reconstruct them — and the delete event carries them too, which it
did not before this release.

### 1.3 Tenant-scoped placement, on every path

A sandbox is scheduled only onto a worker with the same tenant, or onto one with
no tenant at all. That held on the native API and did *not* hold on two other
paths, both fixed here: the async create rebuilt the identity without its tenant,
and the grace-period fallback used an unscoped selector. The E2B gateway now
resolves its own keys (`Config.E2BAPIKeys`) and carries the tenant through to
placement as well.

`RoleWorker` is a new role: the two routes a worker needs (the tunnel handshake
and the job poll), and nothing else. A join token can no longer list the fleet,
create sandboxes or read another tenant's anything.

### 1.4 Sandbox creation is 230 ms, not 4.2 s

The worker pulled the toolbox image before creating every container, even when it
was already on disk. It now inspects first and pulls only when missing.

---

## 2. Upgrading from 0.6.x

Read this before rolling: everything below is what an existing deployment can
notice.

- **Defaults are unchanged.** Nothing in this release turns itself on.
- **Workers before the control plane**, as in 0.6.0. A 0.6 worker ignores the
  tenant header; a 0.7 control plane refuses to place a tenant-scoped sandbox
  onto it rather than silently co-locating two tenants.
- **`/api/v1/poders/{id}/heartbeat` now accepts a worker token** for the worker's
  own record. If you routed that path through an admin-only proxy, workers using
  join tokens will start succeeding where they previously received 403 — and,
  more importantly, will stop going stale.
- **Unknown resource readings are negative.** `cpu_usage`/`memory_usage` in the
  heartbeat are `-1` when the worker cannot read `/proc`, where they used to be a
  confident `0.5`. A dashboard that treated the value as a fraction will show
  `-100%`; clamp at zero or read the sign.

### Breaking Go API changes

An embedding build from 0.6 must adapt:

- `cmd/server` internals moved to `pkg/server`; `main.version` is no longer the
  ldflags symbol — use `-X github.com/sandrpod/sandrpod/pkg/server.version`.
- `server.New` takes `(cfg Config, stores podpkg.Stores, tunnelStore, directStore)`.
- `Scheduler.SelectBestFor(region, provider, require []string, tenant string)`
  replaces the three-argument form; `SelectBest` remains for unscoped callers.
- `SandboxInfo.Owner` is populated on delete events (it was empty before).

---

## 3. Known limits, stated

- **Placement is tenant-scoped, not tenant-isolated at the network layer.** Two
  tenants on the same docker daemon share its bridge; a sandbox on one can reach
  a sandbox IP on the other. The egress policy bounds what leaves a *container*,
  not what it can see inside the bridge. Run one daemon per tenant where that
  matters.
- **The terminal is HTTP/1.1 only** when credential masking terminates TLS, so
  interactive tools that negotiate h2 fall back.
- **No per-sandbox metric history.** The toolbox reports live metrics; a time
  series would need a worker-side sampler.
- **The E2B surface is path-mode opt-in.** Mounting it takes over `/sandboxes`.

---

## 4. How this release was verified

- `go test ./...` on linux/amd64 and darwin/arm64, plus the privileged netns
  suite, which now asserts the proxy's *decision* rather than whether the
  internet answered — a flaky test that failed on a bad runner link and passed on
  rerun was telling us nothing.
- The playground (`hack/playground/run.sh`) builds the sidecar for real and fails
  when the build fails; it had been silently skipping that phase on CI.
- Cold start measured at 230 ms p50 locally, 616 ms on the CI runner.
- Tenant placement verified in both directions: bob cannot see, place onto, or
  delete alice's workers, and an async create keeps its tenant.

---

## 5. 中文摘要

0.7.0 的主线是**可嵌入**：控制面从 `cmd/server` 抽成 `pkg/server`，另一个程序可以
`import` 它，并通过三个 seam（认证、准入、事件）接入自己的逻辑；E2B 兼容面变成可挂载
的 handler，而不是必须运行的二进制。`cmd/server` 行为不变，所有新能力默认关闭。

租户隔离在这版补齐了三条曾经漏掉的路径：异步创建丢 tenant、宽限期回退用了无作用域
选择器、E2B 网关不带 tenant。worker 现在有独立角色 `RoleWorker`（只能握手和领任务），
join token 不再能读舰队视图或创建沙箱。

顺带两个"看着像功能、其实是 bug"的修复：worker 用 join token 时心跳一直 403（在线但
状态不再更新），以及沙箱创建从 4.2s 降到 230ms（每次创建前都无条件拉镜像）。

**升级顺序：先 poder，再 server。** 破坏性 Go API 变更见上；心跳里的未知读数现在是负数，
仪表盘按 0..1 处理会显示 -100%，请钳到 0 或读符号位。
